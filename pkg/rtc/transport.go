// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtc

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/dtls/v3/pkg/crypto/elliptic"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/transport"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/bwe/remotebwe"
	"github.com/livekit/livekit-server/pkg/sfu/bwe/sendsidebwe"
	"github.com/livekit/livekit-server/pkg/sfu/datachannel"
	sfuinterceptor "github.com/livekit/livekit-server/pkg/sfu/interceptor"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	pd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/playoutdelay"
	"github.com/livekit/livekit-server/pkg/sfu/streamallocator"
	sfuutils "github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/pkg/utils"
	lkinterceptor "github.com/livekit/mediatransportutil/pkg/interceptor"
	lktwcc "github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/pionlogger"
	lksdp "github.com/livekit/protocol/sdp"
)

const (
	LossyDataChannel    = "_lossy"
	ReliableDataChannel = "_reliable"

	fastNegotiationFrequency   = 10 * time.Millisecond
	negotiationFrequency       = 150 * time.Millisecond
	negotiationFailedTimeout   = 15 * time.Second
	dtlsRetransmissionInterval = 100 * time.Millisecond

	iceDisconnectedTimeout = 10 * time.Second                          // compatible for ice-lite with firefox client
	iceFailedTimeout       = 5 * time.Second                           // time between disconnected and failed
	iceFailedTimeoutTotal  = iceFailedTimeout + iceDisconnectedTimeout // total time between connecting and failure
	iceKeepaliveInterval   = 2 * time.Second                           // pion's default

	minTcpICEConnectTimeout = 5 * time.Second
	maxTcpICEConnectTimeout = 12 * time.Second // js-sdk has a default 15s timeout for first connection, let server detect failure earlier before that

	minConnectTimeoutAfterICE = 10 * time.Second
	maxConnectTimeoutAfterICE = 20 * time.Second // max duration for waiting pc to connect after ICE is connected

	shortConnectionThreshold = 90 * time.Second

	dataChannelBufferSize = 65535
)

var (
	ErrNoICETransport                    = errors.New("no ICE transport")
	ErrIceRestartWithoutLocalSDP         = errors.New("ICE restart without local SDP settled")
	ErrIceRestartOnClosedPeerConnection  = errors.New("ICE restart on closed peer connection")
	ErrNoTransceiver                     = errors.New("no transceiver")
	ErrNoSender                          = errors.New("no sender")
	ErrMidNotFound                       = errors.New("mid not found")
	ErrNotSynchronousLocalCandidatesMode = errors.New("not using synchronous local candidates mode")
	ErrNoRemoteDescription               = errors.New("no remote description")
	ErrNoLocalDescription                = errors.New("no local description")
	ErrInvalidSDPFragment                = errors.New("invalid sdp fragment")
	ErrNoBundleMid                       = errors.New("could not get bundle mid")
	ErrMidMismatch                       = errors.New("media mid does not match bundle mid")
	ErrICECredentialMismatch             = errors.New("ice credential mismatch")
)

// -------------------------------------------------------------------------

type signal int

const (
	signalICEGatheringComplete signal = iota
	signalLocalICECandidate
	signalRemoteICECandidate
	signalSendOffer
	signalRemoteDescriptionReceived
	signalICERestart
)

func (s signal) String() string {
	switch s {
	case signalICEGatheringComplete:
		return "ICE_GATHERING_COMPLETE"
	case signalLocalICECandidate:
		return "LOCAL_ICE_CANDIDATE"
	case signalRemoteICECandidate:
		return "REMOTE_ICE_CANDIDATE"
	case signalSendOffer:
		return "SEND_OFFER"
	case signalRemoteDescriptionReceived:
		return "REMOTE_DESCRIPTION_RECEIVED"
	case signalICERestart:
		return "ICE_RESTART"
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

// -------------------------------------------------------

type event struct {
	*PCTransport
	signal signal
	data   interface{}
}

func (e event) String() string {
	return fmt.Sprintf("PCTransport:Event{signal: %s, data: %+v}", e.signal, e.data)
}

// -------------------------------------------------------

type wrappedICECandidatePairLogger struct {
	pair *webrtc.ICECandidatePair
}

func (w wrappedICECandidatePairLogger) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if w.pair == nil {
		return nil
	}

	if w.pair.Local != nil {
		e.AddString("localProtocol", w.pair.Local.Protocol.String())
		e.AddString("localCandidateType", w.pair.Local.Typ.String())
		e.AddString("localAdddress", w.pair.Local.Address)
		e.AddUint16("localPort", w.pair.Local.Port)
	}
	if w.pair.Remote != nil {
		e.AddString("remoteProtocol", w.pair.Remote.Protocol.String())
		e.AddString("remoteCandidateType", w.pair.Remote.Typ.String())
		e.AddString("remoteAdddress", MaybeTruncateIP(w.pair.Remote.Address))
		e.AddUint16("remotePort", w.pair.Remote.Port)
		if w.pair.Remote.RelatedAddress != "" {
			e.AddString("relatedAdddress", MaybeTruncateIP(w.pair.Remote.RelatedAddress))
			e.AddUint16("relatedPort", w.pair.Remote.RelatedPort)
		}
	}
	return nil
}

// -------------------------------------------------------------------

type SimulcastTrackInfo struct {
	Mid string
	Rid string
}

type trackDescription struct {
	mid    string
	sender *webrtc.RTPSender
}

// PCTransport is a wrapper around PeerConnection, with some helper methods
type PCTransport struct {
	params       TransportParams
	pc           *webrtc.PeerConnection
	iceTransport *webrtc.ICETransport
	me           *webrtc.MediaEngine

	lock sync.RWMutex

	firstOfferReceived      bool
	firstOfferNoDataChannel bool
	reliableDC              *datachannel.DataChannelWriter[*webrtc.DataChannel]
	reliableDCOpened        bool
	lossyDC                 *datachannel.DataChannelWriter[*webrtc.DataChannel]
	lossyDCOpened           bool
	unlabeledDataChannels   []*datachannel.DataChannelWriter[*webrtc.DataChannel]

	iceStartedAt               time.Time
	iceConnectedAt             time.Time
	firstConnectedAt           time.Time
	connectedAt                time.Time
	tcpICETimer                *time.Timer
	connectAfterICETimer       *time.Timer // timer to wait for pc to connect after ice connected
	resetShortConnOnICERestart atomic.Bool
	signalingRTT               atomic.Uint32 // milliseconds

	debouncedNegotiate *sfuutils.Debouncer
	debouncePending    bool
	lastNegotiate      time.Time

	onNegotiationStateChanged func(state transport.NegotiationState)

	// stream allocator for subscriber PC
	streamAllocator *streamallocator.StreamAllocator

	// only for subscriber PC
	bwe   bwe.BWE
	pacer pacer.Pacer

	// transceivers (senders) waiting for SetRemoteDescription (offer) to happen before
	// SetCodecPreferences can be invoked on them.
	// Pion adapts codecs/payload types from remote description.
	// If SetCodecPreferences are done before the remote desctiption is processed,
	// it is possible that the transceiver gets payload types from media engine.
	// Subssequently if the peer sends an offer with different payload type for the
	// same codec, there could be two payload types for the same codec and the wrong
	// one could be used in the forwarding path. So, wait for `SetRemoteDescription`
	// to happen so that remote side payload types are adapted.
	sendersPendingConfigMu sync.Mutex
	sendersPendingConfig   []configureSenderParams

	previousAnswer *webrtc.SessionDescription
	// track id -> description map in previous offer sdp
	previousTrackDescription map[string]*trackDescription
	canReuseTransceiver      bool

	preferTCP atomic.Bool
	isClosed  atomic.Bool

	// used to check for offer/answer pairing,
	// i. e. every offer should have an answer before another offer can be sent
	localOfferId   atomic.Uint32
	remoteAnswerId atomic.Uint32

	remoteOfferId atomic.Uint32
	localAnswerId atomic.Uint32

	eventsQueue *utils.TypedOpsQueue[event]

	connectionDetails      *types.ICEConnectionDetails
	selectedPair           atomic.Pointer[webrtc.ICECandidatePair]
	mayFailedICEStats      []iceCandidatePairStats
	mayFailedICEStatsTimer *time.Timer

	numOutstandingAudios uint32
	numRequestSentAudios uint32
	numOutstandingVideos uint32
	numRequestSentVideos uint32

	// the following should be accessed only in event processing go routine
	cacheLocalCandidates      bool
	cachedLocalCandidates     []*webrtc.ICECandidate
	pendingRemoteCandidates   []*webrtc.ICECandidateInit
	restartAfterGathering     bool
	restartAtNextOffer        bool
	negotiationState          transport.NegotiationState
	negotiateCounter          atomic.Int32
	signalStateCheckTimer     *time.Timer
	currentOfferIceCredential string // ice user:pwd, for publish side ice restart checking
	pendingRestartIceOffer    *webrtc.SessionDescription
}

type TransportParams struct {
	Handler                      transport.Handler
	ProtocolVersion              types.ProtocolVersion
	Config                       *WebRTCConfig
	Twcc                         *lktwcc.Responder
	DirectionConfig              DirectionConfig
	CongestionControlConfig      config.CongestionControlConfig
	EnabledCodecs                []*livekit.Codec
	Logger                       logger.Logger
	Transport                    livekit.SignalTarget
	SimTracks                    map[uint32]SimulcastTrackInfo
	ClientInfo                   ClientInfo
	IsOfferer                    bool
	IsSendSide                   bool
	AllowPlayoutDelay            bool
	UseOneShotSignallingMode     bool
	FireOnTrackBySdp             bool
	DataChannelMaxBufferedAmount uint64
	DatachannelSlowThreshold     int

	// for development test
	DatachannelMaxReceiverBufferSize int
}

func newPeerConnection(params TransportParams, onBandwidthEstimator func(estimator cc.BandwidthEstimator)) (*webrtc.PeerConnection, *webrtc.MediaEngine, error) {
	directionConfig := params.DirectionConfig
	if params.AllowPlayoutDelay {
		directionConfig.RTPHeaderExtension.Video = append(directionConfig.RTPHeaderExtension.Video, pd.PlayoutDelayURI)
	}

	// Some of the browser clients do not handle H.264 High Profile in signalling properly.
	// They still decode if the actual stream is H.264 High Profile, but do not handle it well in signalling.
	// So, disable H.264 High Profile for SUBSCRIBER peer connection to ensure it is not offered.
	me, err := createMediaEngine(params.EnabledCodecs, directionConfig, params.IsOfferer)
	if err != nil {
		return nil, nil, err
	}

	se := params.Config.SettingEngine
	se.DisableMediaEngineCopy(true)

	// Change elliptic curve to improve connectivity
	// https://github.com/pion/dtls/pull/474
	se.SetDTLSEllipticCurves(elliptic.X25519, elliptic.P384, elliptic.P256)

	// Disable close by dtls to avoid peerconnection close too early in migration
	// https://github.com/pion/webrtc/pull/2961
	se.DisableCloseByDTLS(true)

	se.DetachDataChannels()
	if params.DatachannelSlowThreshold > 0 {
		se.EnableDataChannelBlockWrite(true)
	}
	if params.DatachannelMaxReceiverBufferSize > 0 {
		se.SetSCTPMaxReceiveBufferSize(uint32(params.DatachannelMaxReceiverBufferSize))
	}
	if params.FireOnTrackBySdp {
		se.SetFireOnTrackBeforeFirstRTP(true)
	}

	if params.ClientInfo.SupportsSctpZeroChecksum() {
		se.EnableSCTPZeroChecksum(true)
	}

	//
	// Disable SRTP replay protection (https://datatracker.ietf.org/doc/html/rfc3711#page-15).
	// Needed due to lack of RTX stream support in Pion.
	//
	// When clients probe for bandwidth, there are several possible approaches
	//   1. Use padding packet (Chrome uses this)
	//   2. Use an older packet (Firefox uses this)
	// Typically, these are sent over the RTX stream and hence SRTP replay protection will not
	// trigger. As Pion does not support RTX, when firefox uses older packet for probing, they
	// trigger the replay protection.
	//
	// That results in two issues
	//   - Firefox bandwidth probing is not successful
	//   - Pion runs out of read buffer capacity - this potentially looks like a Pion issue
	//
	// NOTE: It is not required to disable RTCP replay protection, but doing it to be symmetric.
	//
	se.DisableSRTPReplayProtection(true)
	se.DisableSRTCPReplayProtection(true)
	if !params.ProtocolVersion.SupportsICELite() || !params.ClientInfo.SupportsPrflxOverRelay() {
		// if client don't support prflx over relay which is only Firefox, disable ICE Lite to ensure that
		// aggressive nomination is handled properly. Firefox does aggressive nomination even if peer is
		// ICE Lite (see comment as to historical reasons: https://github.com/pion/ice/pull/739#issuecomment-2452245066).
		// pion/ice (as of v2.3.37) will accept all use-candidate switches when in ICE Lite mode.
		// That combined with aggressive nomination from Firefox could potentially lead to the two ends
		// ending up with different candidates.
		// As Firefox does not support migration, ICE Lite can be disabled.
		se.SetLite(false)
	}
	se.SetDTLSRetransmissionInterval(dtlsRetransmissionInterval)
	se.SetICETimeouts(iceDisconnectedTimeout, iceFailedTimeout, iceKeepaliveInterval)

	// if client don't support prflx over relay, we should not expose private address to it, use single external ip as host candidate
	if !params.ClientInfo.SupportsPrflxOverRelay() && len(params.Config.NAT1To1IPs) > 0 {
		var nat1to1Ips []string
		var includeIps []string
		for _, mapping := range params.Config.NAT1To1IPs {
			if ips := strings.Split(mapping, "/"); len(ips) == 2 {
				if ips[0] != ips[1] {
					nat1to1Ips = append(nat1to1Ips, mapping)
					includeIps = append(includeIps, ips[1])
				}
			}
		}
		if len(nat1to1Ips) > 0 {
			params.Logger.Infow("client doesn't support prflx over relay, use external ip only as host candidate", "ips", nat1to1Ips)
			se.SetNAT1To1IPs(nat1to1Ips, webrtc.ICECandidateTypeHost)
			se.SetIPFilter(func(ip net.IP) bool {
				if ip.To4() == nil {
					return true
				}
				ipstr := ip.String()
				for _, inc := range includeIps {
					if inc == ipstr {
						return true
					}
				}
				return false
			})
		}
	}

	lf := pionlogger.NewLoggerFactory(params.Logger)
	if lf != nil {
		se.LoggerFactory = lf
	}

	ir := &interceptor.Registry{}
	if params.IsSendSide {
		if params.CongestionControlConfig.UseSendSideBWEInterceptor && !params.CongestionControlConfig.UseSendSideBWE {
			params.Logger.Infow("using send side BWE - interceptor")
			gf, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
				return gcc.NewSendSideBWE(
					gcc.SendSideBWEInitialBitrate(1*1000*1000),
					gcc.SendSideBWEPacer(gcc.NewNoOpPacer()),
				)
			})
			if err == nil {
				gf.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
					if onBandwidthEstimator != nil {
						onBandwidthEstimator(estimator)
					}
				})
				ir.Add(gf)

				tf, err := twcc.NewHeaderExtensionInterceptor()
				if err == nil {
					ir.Add(tf)
				}
			}
		}
	}
	if !params.IsOfferer {
		// sfu only use interceptor to send XR but don't read response from it (use buffer instead),
		// so use a empty callback here
		ir.Add(lkinterceptor.NewRTTFromXRFactory(func(rtt uint32) {}))
	}
	if len(params.SimTracks) > 0 {
		f, err := NewUnhandleSimulcastInterceptorFactory(UnhandleSimulcastTracks(params.SimTracks))
		if err != nil {
			params.Logger.Warnw("NewUnhandleSimulcastInterceptorFactory failed", err)
		} else {
			ir.Add(f)
		}
	}

	setTWCCForVideo := func(info *interceptor.StreamInfo) {
		if !mime.IsMimeTypeStringVideo(info.MimeType) {
			return
		}
		// rtx stream don't have rtcp feedback, always set twcc for rtx stream
		twccFb := mime.GetMimeTypeCodec(info.MimeType) == mime.MimeTypeCodecRTX
		if !twccFb {
			for _, fb := range info.RTCPFeedback {
				if fb.Type == webrtc.TypeRTCPFBTransportCC {
					twccFb = true
					break
				}
			}
		}
		if !twccFb {
			return
		}

		twccExtID := sfuutils.GetHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI})
		if twccExtID != 0 {
			if buffer := params.Config.BufferFactory.GetBuffer(info.SSRC); buffer != nil {
				params.Logger.Debugw("set rtx twcc and ext id", "ssrc", info.SSRC, "twccExtID", twccExtID)
				buffer.SetTWCCAndExtID(params.Twcc, uint8(twccExtID))
			} else {
				params.Logger.Warnw("failed to get buffer for rtx stream", nil, "ssrc", info.SSRC)
			}
		}
	}
	// put rtx interceptor behind unhandle simulcast interceptor so it can get the correct mid & rid
	ir.Add(sfuinterceptor.NewRTXInfoExtractorFactory(setTWCCForVideo, func(repair, base uint32) {
		params.Logger.Debugw("rtx pair found from extension", "repair", repair, "base", base)
		params.Config.BufferFactory.SetRTXPair(repair, base)
	}, params.Logger))

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(ir),
	)
	pc, err := api.NewPeerConnection(params.Config.Configuration)
	return pc, me, err
}

func NewPCTransport(params TransportParams) (*PCTransport, error) {
	if params.Logger == nil {
		params.Logger = logger.GetLogger()
	}
	t := &PCTransport{
		params:             params,
		debouncedNegotiate: sfuutils.NewDebouncer(negotiationFrequency),
		negotiationState:   transport.NegotiationStateNone,
		eventsQueue: utils.NewTypedOpsQueue[event](utils.OpsQueueParams{
			Name:    "transport",
			MinSize: 64,
			Logger:  params.Logger,
		}),
		previousTrackDescription: make(map[string]*trackDescription),
		canReuseTransceiver:      true,
		connectionDetails:        types.NewICEConnectionDetails(params.Transport, params.Logger),
		lastNegotiate:            time.Now(),
	}
	t.localOfferId.Store(uint32(rand.Intn(1<<8) + 1))

	bwe, err := t.createPeerConnection()
	if err != nil {
		return nil, err
	}

	if params.IsSendSide {
		if params.CongestionControlConfig.UseSendSideBWE {
			params.Logger.Infow("using send side BWE", "pacerBehavior", params.CongestionControlConfig.SendSideBWEPacer)
			t.bwe = sendsidebwe.NewSendSideBWE(sendsidebwe.SendSideBWEParams{
				Config: params.CongestionControlConfig.SendSideBWE,
				Logger: params.Logger,
			})
			switch pacer.PacerBehavior(params.CongestionControlConfig.SendSideBWEPacer) {
			case pacer.PacerBehaviorPassThrough:
				t.pacer = pacer.NewPassThrough(params.Logger, t.bwe)
			case pacer.PacerBehaviorNoQueue:
				t.pacer = pacer.NewNoQueue(params.Logger, t.bwe)
			default:
				t.pacer = pacer.NewNoQueue(params.Logger, t.bwe)
			}
		} else {
			t.bwe = remotebwe.NewRemoteBWE(remotebwe.RemoteBWEParams{
				Config: params.CongestionControlConfig.RemoteBWE,
				Logger: params.Logger,
			})
			t.pacer = pacer.NewPassThrough(params.Logger, nil)
		}

		t.streamAllocator = streamallocator.NewStreamAllocator(streamallocator.StreamAllocatorParams{
			Config:    params.CongestionControlConfig.StreamAllocator,
			BWE:       t.bwe,
			Pacer:     t.pacer,
			RTTGetter: t.GetRTT,
			Logger:    params.Logger.WithComponent(utils.ComponentCongestionControl),
		}, params.CongestionControlConfig.Enabled, params.CongestionControlConfig.AllowPause)
		t.streamAllocator.OnStreamStateChange(params.Handler.OnStreamStateChange)
		t.streamAllocator.Start()

		if bwe != nil {
			t.streamAllocator.SetSendSideBWEInterceptor(bwe)
		}
	}

	t.eventsQueue.Start()

	return t, nil
}

func (t *PCTransport) createPeerConnection() (cc.BandwidthEstimator, error) {
	var bwe cc.BandwidthEstimator
	pc, me, err := newPeerConnection(t.params, func(estimator cc.BandwidthEstimator) {
		bwe = estimator
	})
	if err != nil {
		return bwe, err
	}

	t.pc = pc
	if !t.params.UseOneShotSignallingMode {
		// one shot signalling mode gathers all candidates and sends in answer
		t.pc.OnICEGatheringStateChange(t.onICEGatheringStateChange)
		t.pc.OnICECandidate(t.onICECandidateTrickle)
	}
	t.pc.OnICEConnectionStateChange(t.onICEConnectionStateChange)
	t.pc.OnConnectionStateChange(t.onPeerConnectionStateChange)

	t.pc.OnDataChannel(t.onDataChannel)
	t.pc.OnTrack(t.params.Handler.OnTrack)

	t.iceTransport = t.pc.SCTP().Transport().ICETransport()
	if t.iceTransport == nil {
		return bwe, ErrNoICETransport
	}
	t.iceTransport.OnSelectedCandidatePairChange(func(pair *webrtc.ICECandidatePair) {
		t.params.Logger.Debugw("selected ICE candidate pair changed", "pair", wrappedICECandidatePairLogger{pair})
		t.connectionDetails.SetSelectedPair(pair)
		existingPair := t.selectedPair.Load()
		if existingPair != nil {
			t.params.Logger.Infow(
				"ice reconnected or switched pair",
				"existingPair", wrappedICECandidatePairLogger{existingPair},
				"newPair", wrappedICECandidatePairLogger{pair})
		}
		t.selectedPair.Store(pair)
	})

	t.me = me
	return bwe, nil
}

func (t *PCTransport) GetPacer() pacer.Pacer {
	return t.pacer
}

func (t *PCTransport) SetSignalingRTT(rtt uint32) {
	t.signalingRTT.Store(rtt)
}

func (t *PCTransport) setICEStartedAt(at time.Time) {
	t.lock.Lock()
	if t.iceStartedAt.IsZero() {
		t.iceStartedAt = at

		// checklist of ice agent will be cleared on ice failed, get stats before that
		t.mayFailedICEStatsTimer = time.AfterFunc(iceFailedTimeoutTotal-time.Second, t.logMayFailedICEStats)

		// set failure timer for tcp ice connection based on signaling RTT
		if t.preferTCP.Load() {
			signalingRTT := t.signalingRTT.Load()
			if signalingRTT < 1000 {
				tcpICETimeout := time.Duration(signalingRTT*8) * time.Millisecond
				if tcpICETimeout < minTcpICEConnectTimeout {
					tcpICETimeout = minTcpICEConnectTimeout
				} else if tcpICETimeout > maxTcpICEConnectTimeout {
					tcpICETimeout = maxTcpICEConnectTimeout
				}
				t.params.Logger.Debugw("set TCP ICE connect timer", "timeout", tcpICETimeout, "signalRTT", signalingRTT)
				t.tcpICETimer = time.AfterFunc(tcpICETimeout, func() {
					if t.pc.ICEConnectionState() == webrtc.ICEConnectionStateChecking {
						t.params.Logger.Infow("TCP ICE connect timeout", "timeout", tcpICETimeout, "signalRTT", signalingRTT)
						t.logMayFailedICEStats()
						t.handleConnectionFailed(true)
					}
				})
			}
		}
	}
	t.lock.Unlock()
}

func (t *PCTransport) setICEConnectedAt(at time.Time) {
	t.lock.Lock()
	if t.iceConnectedAt.IsZero() {
		//
		// Record initial connection time.
		// This prevents reset of connected at time if ICE goes `Connected` -> `Disconnected` -> `Connected`.
		//
		t.iceConnectedAt = at

		// set failure timer for dtls handshake
		iceDuration := at.Sub(t.iceStartedAt)
		connTimeoutAfterICE := minConnectTimeoutAfterICE
		if connTimeoutAfterICE < 3*iceDuration {
			connTimeoutAfterICE = 3 * iceDuration
		}
		if connTimeoutAfterICE > maxConnectTimeoutAfterICE {
			connTimeoutAfterICE = maxConnectTimeoutAfterICE
		}
		t.params.Logger.Debugw("setting connection timer after ICE connected", "timeout", connTimeoutAfterICE, "iceDuration", iceDuration)
		t.connectAfterICETimer = time.AfterFunc(connTimeoutAfterICE, func() {
			state := t.pc.ConnectionState()
			// if pc is still checking or connected but not fully established after timeout, then fire connection fail
			if state != webrtc.PeerConnectionStateClosed && state != webrtc.PeerConnectionStateFailed && !t.isFullyEstablished() {
				t.params.Logger.Infow("connect timeout after ICE connected", "timeout", connTimeoutAfterICE, "iceDuration", iceDuration)
				t.handleConnectionFailed(false)
			}
		})

		// clear tcp ice connect timer
		if t.tcpICETimer != nil {
			t.tcpICETimer.Stop()
			t.tcpICETimer = nil
		}
	}

	if t.mayFailedICEStatsTimer != nil {
		t.mayFailedICEStatsTimer.Stop()
		t.mayFailedICEStatsTimer = nil
	}
	t.mayFailedICEStats = nil
	t.lock.Unlock()
}

func (t *PCTransport) logMayFailedICEStats() {
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return
	}

	var candidatePairStats []webrtc.ICECandidatePairStats
	pairStats := t.pc.GetStats()
	candidateStats := make(map[string]webrtc.ICECandidateStats)
	for _, stat := range pairStats {
		switch stat := stat.(type) {
		case webrtc.ICECandidatePairStats:
			candidatePairStats = append(candidatePairStats, stat)
		case webrtc.ICECandidateStats:
			candidateStats[stat.ID] = stat
		}
	}

	iceStats := make([]iceCandidatePairStats, 0, len(candidatePairStats))
	for _, pairStat := range candidatePairStats {
		iceStat := iceCandidatePairStats{ICECandidatePairStats: pairStat}
		if local, ok := candidateStats[pairStat.LocalCandidateID]; ok {
			iceStat.local = local
		}
		if remote, ok := candidateStats[pairStat.RemoteCandidateID]; ok {
			remote.IP = MaybeTruncateIP(remote.IP)
			iceStat.remote = remote
		}
		iceStats = append(iceStats, iceStat)
	}

	t.lock.Lock()
	t.mayFailedICEStats = iceStats
	t.lock.Unlock()
}

func (t *PCTransport) resetShortConn() {
	t.params.Logger.Infow("resetting short connection on ICE restart")
	t.lock.Lock()
	t.iceStartedAt = time.Time{}
	t.iceConnectedAt = time.Time{}
	t.connectedAt = time.Time{}
	if t.connectAfterICETimer != nil {
		t.connectAfterICETimer.Stop()
		t.connectAfterICETimer = nil
	}
	if t.tcpICETimer != nil {
		t.tcpICETimer.Stop()
		t.tcpICETimer = nil
	}
	t.lock.Unlock()
}

func (t *PCTransport) IsShortConnection(at time.Time) (bool, time.Duration) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if t.iceConnectedAt.IsZero() {
		return false, 0
	}

	duration := at.Sub(t.iceConnectedAt)
	return duration < shortConnectionThreshold, duration
}

func (t *PCTransport) setConnectedAt(at time.Time) bool {
	t.lock.Lock()
	t.connectedAt = at
	if !t.firstConnectedAt.IsZero() {
		t.lock.Unlock()
		return false
	}

	t.firstConnectedAt = at
	prometheus.RecordServiceOperationSuccess("peer_connection")
	t.lock.Unlock()
	return true
}

func (t *PCTransport) onICEGatheringStateChange(state webrtc.ICEGatheringState) {
	t.params.Logger.Debugw("ice gathering state change", "state", state.String())
	if state != webrtc.ICEGatheringStateComplete {
		return
	}

	t.postEvent(event{
		signal: signalICEGatheringComplete,
	})
}

func (t *PCTransport) onICECandidateTrickle(c *webrtc.ICECandidate) {
	t.postEvent(event{
		signal: signalLocalICECandidate,
		data:   c,
	})
}

func (t *PCTransport) handleConnectionFailed(forceShortConn bool) {
	isShort := forceShortConn
	if !isShort {
		var duration time.Duration
		isShort, duration = t.IsShortConnection(time.Now())
		if isShort {
			t.params.Logger.Debugw("short ICE connection", "pair", wrappedICECandidatePairLogger{t.selectedPair.Load()}, "duration", duration)
		}
	}

	t.params.Handler.OnFailed(isShort, t.GetICEConnectionInfo())
}

func (t *PCTransport) onICEConnectionStateChange(state webrtc.ICEConnectionState) {
	t.params.Logger.Debugw("ice connection state change", "state", state.String())
	switch state {
	case webrtc.ICEConnectionStateConnected:
		t.setICEConnectedAt(time.Now())

	case webrtc.ICEConnectionStateChecking:
		t.setICEStartedAt(time.Now())
	}
}

func (t *PCTransport) onPeerConnectionStateChange(state webrtc.PeerConnectionState) {
	t.params.Logger.Debugw("peer connection state change", "state", state.String())
	switch state {
	case webrtc.PeerConnectionStateConnected:
		t.clearConnTimer()
		isInitialConnection := t.setConnectedAt(time.Now())
		if isInitialConnection {
			t.params.Handler.OnInitialConnected()

			t.maybeNotifyFullyEstablished()
		}
	case webrtc.PeerConnectionStateFailed:
		t.clearConnTimer()
		t.handleConnectionFailed(false)
	}
}

func (t *PCTransport) onDataChannel(dc *webrtc.DataChannel) {
	dc.OnOpen(func() {
		t.params.Logger.Debugw(dc.Label() + " data channel open")
		var kind livekit.DataPacket_Kind
		var isUnlabeled bool
		switch dc.Label() {
		case ReliableDataChannel:
			kind = livekit.DataPacket_RELIABLE

		case LossyDataChannel:
			kind = livekit.DataPacket_LOSSY

		default:
			t.params.Logger.Infow("unlabeled datachannel added", "label", dc.Label())
			isUnlabeled = true
		}

		rawDC, err := dc.DetachWithDeadline()
		if err != nil {
			t.params.Logger.Errorw("failed to detach data channel", err, "label", dc.Label())
			return
		}

		switch {
		case isUnlabeled:
			t.lock.Lock()
			t.unlabeledDataChannels = append(
				t.unlabeledDataChannels,
				datachannel.NewDataChannelWriter(dc, rawDC, t.params.DatachannelSlowThreshold),
			)
			t.lock.Unlock()

		case kind == livekit.DataPacket_RELIABLE:
			t.lock.Lock()
			if t.reliableDC != nil {
				t.reliableDC.Close()
			}
			t.reliableDC = datachannel.NewDataChannelWriter(dc, rawDC, t.params.DatachannelSlowThreshold)
			t.reliableDCOpened = true
			t.lock.Unlock()

		case kind == livekit.DataPacket_LOSSY:
			t.lock.Lock()
			if t.lossyDC != nil {
				t.lossyDC.Close()
			}
			t.lossyDC = datachannel.NewDataChannelWriter(dc, rawDC, 0)
			t.lossyDCOpened = true
			t.lock.Unlock()
		}

		go func() {
			defer rawDC.Close()
			buffer := make([]byte, dataChannelBufferSize)
			for {
				n, _, err := rawDC.ReadDataChannel(buffer)
				if err != nil {
					if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "state=Closed") {
						t.params.Logger.Warnw("error reading data channel", err, "label", dc.Label())
					}
					return
				}

				switch {
				case isUnlabeled:
					t.params.Handler.OnDataMessageUnlabeled(buffer[:n])

				default:
					t.params.Handler.OnDataMessage(kind, buffer[:n])
				}
			}
		}()

		t.maybeNotifyFullyEstablished()
	})
}

func (t *PCTransport) maybeNotifyFullyEstablished() {
	if t.isFullyEstablished() {
		t.params.Handler.OnFullyEstablished()
	}
}

func (t *PCTransport) isFullyEstablished() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	dataChannelReady := t.params.UseOneShotSignallingMode || t.firstOfferNoDataChannel || (t.reliableDCOpened && t.lossyDCOpened)

	return dataChannelReady && !t.connectedAt.IsZero()
}

func (t *PCTransport) SetPreferTCP(preferTCP bool) {
	t.preferTCP.Store(preferTCP)
}

func (t *PCTransport) AddICECandidate(candidate webrtc.ICECandidateInit) {
	t.postEvent(event{
		signal: signalRemoteICECandidate,
		data:   &candidate,
	})
}

func (t *PCTransport) queueOrConfigureSender(
	transceiver *webrtc.RTPTransceiver,
	enabledCodecs []*livekit.Codec,
	rtcpFeedbackConfig RTCPFeedbackConfig,
	enableAudioStereo bool,
	enableAudioNACK bool,
) {
	params := configureSenderParams{
		transceiver,
		enabledCodecs,
		rtcpFeedbackConfig,
		!t.params.IsOfferer,
		enableAudioStereo,
		enableAudioNACK,
	}
	if !t.params.IsOfferer {
		t.sendersPendingConfigMu.Lock()
		t.sendersPendingConfig = append(t.sendersPendingConfig, params)
		t.sendersPendingConfigMu.Unlock()
		return
	}

	configureSender(params)
}

func (t *PCTransport) processSendersPendingConfig() {
	t.sendersPendingConfigMu.Lock()
	pending := t.sendersPendingConfig
	t.sendersPendingConfig = nil
	t.sendersPendingConfigMu.Unlock()

	var unprocessed []configureSenderParams
	for _, p := range pending {
		if p.transceiver.Mid() == "" {
			unprocessed = append(unprocessed, p)
			continue
		}

		configureSender(p)
	}

	if len(unprocessed) != 0 {
		t.sendersPendingConfigMu.Lock()
		t.sendersPendingConfig = append(t.sendersPendingConfig, unprocessed...)
		t.sendersPendingConfigMu.Unlock()
	}
}

func (t *PCTransport) AddTrack(
	trackLocal webrtc.TrackLocal,
	params types.AddTrackParams,
	enabledCodecs []*livekit.Codec,
	rtcpFeedbackConfig RTCPFeedbackConfig,
) (sender *webrtc.RTPSender, transceiver *webrtc.RTPTransceiver, err error) {
	t.lock.Lock()
	canReuse := t.canReuseTransceiver
	td, ok := t.previousTrackDescription[trackLocal.ID()]
	if ok {
		delete(t.previousTrackDescription, trackLocal.ID())
	}
	t.lock.Unlock()

	// keep track use same mid after migration if possible
	if td != nil && td.sender != nil {
		for _, tr := range t.pc.GetTransceivers() {
			if tr.Mid() == td.mid {
				return td.sender, tr, tr.SetSender(td.sender, trackLocal)
			}
		}
	}

	// if never negotiated with client, can't reuse transceiver for track not subscribed before migration
	if !canReuse {
		return t.AddTransceiverFromTrack(trackLocal, params, enabledCodecs, rtcpFeedbackConfig)
	}

	sender, err = t.pc.AddTrack(trackLocal)
	if err != nil {
		return
	}

	for _, tr := range t.pc.GetTransceivers() {
		if tr.Sender() == sender {
			transceiver = tr
			break
		}
	}

	if transceiver == nil {
		err = ErrNoTransceiver
		return
	}

	t.queueOrConfigureSender(
		transceiver,
		enabledCodecs,
		rtcpFeedbackConfig,
		params.Stereo,
		!params.Red || !t.params.ClientInfo.SupportsAudioRED(),
	)

	t.adjustNumOutstandingMedia(transceiver)
	return
}

func (t *PCTransport) AddTransceiverFromTrack(
	trackLocal webrtc.TrackLocal,
	params types.AddTrackParams,
	enabledCodecs []*livekit.Codec,
	rtcpFeedbackConfig RTCPFeedbackConfig,
) (sender *webrtc.RTPSender, transceiver *webrtc.RTPTransceiver, err error) {
	transceiver, err = t.pc.AddTransceiverFromTrack(trackLocal)
	if err != nil {
		return
	}

	sender = transceiver.Sender()
	if sender == nil {
		err = ErrNoSender
		return
	}

	t.queueOrConfigureSender(
		transceiver,
		enabledCodecs,
		rtcpFeedbackConfig,
		params.Stereo,
		!params.Red || !t.params.ClientInfo.SupportsAudioRED(),
	)

	t.adjustNumOutstandingMedia(transceiver)
	return
}

func (t *PCTransport) AddTransceiverFromKind(
	kind webrtc.RTPCodecType,
	init webrtc.RTPTransceiverInit,
) (*webrtc.RTPTransceiver, error) {
	return t.pc.AddTransceiverFromKind(kind, init)
}

func (t *PCTransport) RemoveTrack(sender *webrtc.RTPSender) error {
	return t.pc.RemoveTrack(sender)
}

func (t *PCTransport) CurrentLocalDescription() *webrtc.SessionDescription {
	cld := t.pc.CurrentLocalDescription()
	if cld == nil {
		return nil
	}

	ld := *cld
	return &ld
}

func (t *PCTransport) CurrentRemoteDescription() *webrtc.SessionDescription {
	crd := t.pc.CurrentRemoteDescription()
	if crd == nil {
		return nil
	}

	rd := *crd
	return &rd
}

func (t *PCTransport) PendingRemoteDescription() *webrtc.SessionDescription {
	prd := t.pc.PendingRemoteDescription()
	if prd == nil {
		return nil
	}

	rd := *prd
	return &rd
}

func (t *PCTransport) GetMid(rtpReceiver *webrtc.RTPReceiver) string {
	for _, tr := range t.pc.GetTransceivers() {
		if tr.Receiver() == rtpReceiver {
			return tr.Mid()
		}
	}

	return ""
}

func (t *PCTransport) GetRTPTransceiver(mid string) *webrtc.RTPTransceiver {
	for _, tr := range t.pc.GetTransceivers() {
		if tr.Mid() == mid {
			return tr
		}
	}

	return nil
}

func (t *PCTransport) GetRTPReceiver(mid string) *webrtc.RTPReceiver {
	for _, tr := range t.pc.GetTransceivers() {
		if tr.Mid() == mid {
			return tr.Receiver()
		}
	}

	return nil
}

func (t *PCTransport) getNumUnmatchedTransceivers() (uint32, uint32) {
	if t.isClosed.Load() || t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return 0, 0
	}

	numAudios := uint32(0)
	numVideos := uint32(0)
	for _, tr := range t.pc.GetTransceivers() {
		if tr.Mid() != "" {
			continue
		}

		switch tr.Kind() {
		case webrtc.RTPCodecTypeAudio:
			numAudios++

		case webrtc.RTPCodecTypeVideo:
			numVideos++
		}
	}

	return numAudios, numVideos
}

func (t *PCTransport) CreateDataChannel(label string, dci *webrtc.DataChannelInit) error {
	dc, err := t.pc.CreateDataChannel(label, dci)
	if err != nil {
		return err
	}
	var (
		dcPtr       **datachannel.DataChannelWriter[*webrtc.DataChannel]
		dcReady     *bool
		isUnlabeled bool
		kind        livekit.DataPacket_Kind
	)
	switch dc.Label() {
	default:
		isUnlabeled = true
		t.params.Logger.Infow("unlabeled datachannel added", "label", dc.Label())
	case ReliableDataChannel:
		dcPtr = &t.reliableDC
		dcReady = &t.reliableDCOpened
		kind = livekit.DataPacket_RELIABLE
	case LossyDataChannel:
		dcPtr = &t.lossyDC
		dcReady = &t.lossyDCOpened
		kind = livekit.DataPacket_LOSSY
	}

	dc.OnOpen(func() {
		rawDC, err := dc.DetachWithDeadline()
		if err != nil {
			t.params.Logger.Warnw("failed to detach data channel", err)
			return
		}

		var slowThreshold int
		if dc.Label() == ReliableDataChannel || isUnlabeled {
			slowThreshold = t.params.DatachannelSlowThreshold
		}

		t.lock.Lock()
		if isUnlabeled {
			t.unlabeledDataChannels = append(
				t.unlabeledDataChannels,
				datachannel.NewDataChannelWriter(dc, rawDC, slowThreshold),
			)
		} else {
			if *dcPtr != nil {
				(*dcPtr).Close()
			}
			*dcPtr = datachannel.NewDataChannelWriter(dc, rawDC, slowThreshold)
			*dcReady = true
		}
		t.lock.Unlock()
		t.params.Logger.Debugw(dc.Label() + " data channel open")

		go func() {
			defer rawDC.Close()
			buffer := make([]byte, dataChannelBufferSize)
			for {
				n, _, err := rawDC.ReadDataChannel(buffer)
				if err != nil {
					if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "state=Closed") {
						t.params.Logger.Warnw("error reading data channel", err, "label", dc.Label())
					}
					return
				}

				switch {
				case isUnlabeled:
					t.params.Handler.OnDataMessageUnlabeled(buffer[:n])

				default:
					t.params.Handler.OnDataMessage(kind, buffer[:n])
				}
			}
		}()

		t.maybeNotifyFullyEstablished()
	})

	return nil
}

// for testing only
func (t *PCTransport) CreateReadableDataChannel(label string, dci *webrtc.DataChannelInit) error {
	dc, err := t.pc.CreateDataChannel(label, dci)
	if err != nil {
		return err
	}

	dc.OnOpen(func() {
		t.params.Logger.Debugw(dc.Label() + " data channel open")
		rawDC, err := dc.DetachWithDeadline()
		if err != nil {
			t.params.Logger.Errorw("failed to detach data channel", err, "label", dc.Label())
			return
		}

		t.lock.Lock()
		t.unlabeledDataChannels = append(
			t.unlabeledDataChannels,
			datachannel.NewDataChannelWriter(dc, rawDC, t.params.DatachannelSlowThreshold),
		)
		t.lock.Unlock()

		go func() {
			defer rawDC.Close()
			buffer := make([]byte, dataChannelBufferSize)
			for {
				n, _, err := rawDC.ReadDataChannel(buffer)
				if err != nil {
					if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "state=Closed") {
						t.params.Logger.Warnw("error reading data channel", err, "label", dc.Label())
					}
					return
				}

				t.params.Handler.OnDataMessageUnlabeled(buffer[:n])
			}
		}()
	})
	return nil
}

func (t *PCTransport) CreateDataChannelIfEmpty(dcLabel string, dci *webrtc.DataChannelInit) (label string, id uint16, existing bool, err error) {
	t.lock.RLock()
	var dcw *datachannel.DataChannelWriter[*webrtc.DataChannel]
	switch dcLabel {
	case ReliableDataChannel:
		dcw = t.reliableDC
	case LossyDataChannel:
		dcw = t.lossyDC
	default:
		t.params.Logger.Warnw("unknown data channel label", nil, "label", label)
		err = errors.New("unknown data channel label")
	}
	t.lock.RUnlock()
	if err != nil {
		return
	}

	if dcw != nil {
		dc := dcw.BufferedAmountGetter()
		return dc.Label(), *dc.ID(), true, nil
	}

	dc, err := t.pc.CreateDataChannel(dcLabel, dci)
	if err != nil {
		return
	}

	t.onDataChannel(dc)
	return dc.Label(), *dc.ID(), false, nil
}

func (t *PCTransport) GetRTT() (float64, bool) {
	scps, ok := t.iceTransport.GetSelectedCandidatePairStats()
	if !ok {
		return 0.0, false
	}

	return scps.CurrentRoundTripTime, true
}

func (t *PCTransport) IsEstablished() bool {
	return t.pc.ConnectionState() != webrtc.PeerConnectionStateNew
}

func (t *PCTransport) HasEverConnected() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return !t.firstConnectedAt.IsZero()
}

func (t *PCTransport) GetICEConnectionInfo() *types.ICEConnectionInfo {
	return t.connectionDetails.GetInfo()
}

func (t *PCTransport) GetICEConnectionType() types.ICEConnectionType {
	return t.connectionDetails.GetConnectionType()
}

func (t *PCTransport) WriteRTCP(pkts []rtcp.Packet) error {
	return t.pc.WriteRTCP(pkts)
}

func (t *PCTransport) SendDataMessage(kind livekit.DataPacket_Kind, data []byte) error {
	convertFromUserPacket := false
	var dc *datachannel.DataChannelWriter[*webrtc.DataChannel]
	t.lock.RLock()
	if t.params.UseOneShotSignallingMode {
		if len(t.unlabeledDataChannels) > 0 {
			// use the first unlabeled to send
			dc = t.unlabeledDataChannels[0]
		}
		convertFromUserPacket = true
	} else {
		if kind == livekit.DataPacket_RELIABLE {
			dc = t.reliableDC
		} else {
			dc = t.lossyDC
		}
	}
	t.lock.RUnlock()

	if convertFromUserPacket {
		dp := &livekit.DataPacket{}
		if err := proto.Unmarshal(data, dp); err != nil {
			return err
		}

		switch payload := dp.Value.(type) {
		case *livekit.DataPacket_User:
			return t.sendDataMessage(dc, payload.User.Payload)
		default:
			return errors.New("cannot forward non user data packet")
		}
	}

	return t.sendDataMessage(dc, data)
}

func (t *PCTransport) SendDataMessageUnlabeled(data []byte, useRaw bool, sender livekit.ParticipantIdentity) error {
	convertToUserPacket := false
	var dc *datachannel.DataChannelWriter[*webrtc.DataChannel]
	t.lock.RLock()
	if t.params.UseOneShotSignallingMode || useRaw {
		if len(t.unlabeledDataChannels) > 0 {
			// use the first unlabeled to send
			dc = t.unlabeledDataChannels[0]
		}
	} else {
		if t.reliableDC != nil {
			dc = t.reliableDC
		} else if t.lossyDC != nil {
			dc = t.lossyDC
		}

		convertToUserPacket = true
	}
	t.lock.RUnlock()

	if convertToUserPacket {
		dpData, err := proto.Marshal(&livekit.DataPacket{
			ParticipantIdentity: string(sender),
			Value: &livekit.DataPacket_User{
				User: &livekit.UserPacket{Payload: data},
			},
		})
		if err != nil {
			return err
		}
		return t.sendDataMessage(dc, dpData)
	}

	return t.sendDataMessage(dc, data)
}

func (t *PCTransport) sendDataMessage(dc *datachannel.DataChannelWriter[*webrtc.DataChannel], data []byte) error {
	if dc == nil {
		return ErrDataChannelUnavailable
	}

	if t.pc.ConnectionState() == webrtc.PeerConnectionStateFailed {
		return ErrTransportFailure
	}

	if t.params.DatachannelSlowThreshold == 0 && t.params.DataChannelMaxBufferedAmount > 0 && dc.BufferedAmountGetter().BufferedAmount() > t.params.DataChannelMaxBufferedAmount {
		return ErrDataChannelBufferFull
	}
	_, err := dc.Write(data)

	return err
}

func (t *PCTransport) Close() {
	if t.isClosed.Swap(true) {
		return
	}

	<-t.eventsQueue.Stop()
	t.clearSignalStateCheckTimer()

	if t.streamAllocator != nil {
		t.streamAllocator.Stop()
	}
	if t.pacer != nil {
		t.pacer.Stop()
	}

	t.clearConnTimer()

	t.lock.Lock()
	if t.mayFailedICEStatsTimer != nil {
		t.mayFailedICEStatsTimer.Stop()
		t.mayFailedICEStatsTimer = nil
	}

	if t.reliableDC != nil {
		t.reliableDC.Close()
		t.reliableDC = nil
	}

	if t.lossyDC != nil {
		t.lossyDC.Close()
		t.lossyDC = nil
	}

	for _, dc := range t.unlabeledDataChannels {
		dc.Close()
	}
	t.unlabeledDataChannels = nil
	t.lock.Unlock()

	_ = t.pc.Close()

	t.outputAndClearICEStats()
}

func (t *PCTransport) clearConnTimer() {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.connectAfterICETimer != nil {
		t.connectAfterICETimer.Stop()
		t.connectAfterICETimer = nil
	}

	if t.tcpICETimer != nil {
		t.tcpICETimer.Stop()
		t.tcpICETimer = nil
	}
}

func (t *PCTransport) HandleRemoteDescription(sd webrtc.SessionDescription, remoteId uint32) error {
	if t.params.UseOneShotSignallingMode {
		if sd.Type == webrtc.SDPTypeOffer {
			remoteOfferId := t.remoteOfferId.Load()
			if remoteOfferId != 0 && remoteOfferId != t.localAnswerId.Load() {
				t.params.Logger.Warnw(
					"sdp state: multiple offers without answer", nil,
					"remoteOfferId", remoteOfferId,
					"localAnswerId", t.localAnswerId.Load(),
					"receivedRemoteOfferId", remoteId,
				)
			}
			t.remoteOfferId.Store(remoteId)
		} else {
			if remoteId != 0 && remoteId != t.localOfferId.Load() {
				t.params.Logger.Warnw("sdp state: answer id mismatch", nil, "expected", t.localOfferId.Load(), "got", remoteId)
			}
			t.remoteAnswerId.Store(remoteId)
		}

		// add remote candidates to ICE connection details
		parsed, err := sd.Unmarshal()
		if err == nil {
			addRemoteICECandidates := func(attrs []sdp.Attribute) {
				for _, a := range attrs {
					if a.IsICECandidate() {
						c, err := ice.UnmarshalCandidate(a.Value)
						if err != nil {
							continue
						}
						t.connectionDetails.AddRemoteICECandidate(c, false, false, false)
					}
				}
			}

			addRemoteICECandidates(parsed.Attributes)
			for _, m := range parsed.MediaDescriptions {
				addRemoteICECandidates(m.Attributes)
			}
		}

		err = t.pc.SetRemoteDescription(sd)
		if err != nil {
			t.params.Logger.Errorw("could not set remote description on synchronous mode peer connection", err)
			return err
		}

		rtxRepairs := nonSimulcastRTXRepairsFromSDP(parsed, t.params.Logger)
		if len(rtxRepairs) > 0 {
			t.params.Logger.Debugw("rtx pairs found from sdp", "ssrcs", rtxRepairs)
			for repair, base := range rtxRepairs {
				t.params.Config.BufferFactory.SetRTXPair(repair, base)
			}
		}
		return nil
	}

	t.postEvent(event{
		signal: signalRemoteDescriptionReceived,
		data: remoteDescriptionData{
			sessionDescription: &sd,
			remoteId:           remoteId,
		},
	})
	return nil
}

func (t *PCTransport) GetAnswer() (webrtc.SessionDescription, uint32, error) {
	if !t.params.UseOneShotSignallingMode {
		return webrtc.SessionDescription{}, 0, ErrNotSynchronousLocalCandidatesMode
	}

	prd := t.pc.PendingRemoteDescription()
	if prd == nil || prd.Type != webrtc.SDPTypeOffer {
		return webrtc.SessionDescription{}, 0, ErrNoRemoteDescription
	}

	answer, err := t.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, 0, err
	}

	if err = t.pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, 0, err
	}

	// wait for gathering to complete to include all candidates in the answer
	<-webrtc.GatheringCompletePromise(t.pc)

	cld := t.pc.CurrentLocalDescription()

	// add local candidates to ICE connection details
	parsed, err := cld.Unmarshal()
	if err == nil {
		addLocalICECandidates := func(attrs []sdp.Attribute) {
			for _, a := range attrs {
				if a.IsICECandidate() {
					c, err := ice.UnmarshalCandidate(a.Value)
					if err != nil {
						continue
					}
					t.connectionDetails.AddLocalICECandidate(c, false, false)
				}
			}
		}

		addLocalICECandidates(parsed.Attributes)
		for _, m := range parsed.MediaDescriptions {
			addLocalICECandidates(m.Attributes)
		}
	}

	answerId := t.remoteOfferId.Load()
	t.localAnswerId.Store(answerId)

	return *cld, answerId, nil
}

func (t *PCTransport) GetICESessionUfrag() (string, error) {
	cld := t.pc.CurrentLocalDescription()
	if cld == nil {
		return "", ErrNoLocalDescription
	}

	parsed, err := cld.Unmarshal()
	if err != nil {
		return "", err
	}

	ufrag, _, err := lksdp.ExtractICECredential(parsed)
	if err != nil {
		return "", err
	}

	return ufrag, nil
}

// Handles SDP Fragment for ICE Trickle in WHIP
func (t *PCTransport) HandleICETrickleSDPFragment(sdpFragment string) error {
	if !t.params.UseOneShotSignallingMode {
		return ErrNotSynchronousLocalCandidatesMode
	}

	parsedFragment := &lksdp.SDPFragment{}
	if err := parsedFragment.Unmarshal(sdpFragment); err != nil {
		t.params.Logger.Warnw("could not parse SDP fragment", err, "sdpFragment", sdpFragment)
		return ErrInvalidSDPFragment
	}

	crd := t.pc.CurrentRemoteDescription()
	if crd == nil {
		t.params.Logger.Warnw("no remote description", nil)
		return ErrNoRemoteDescription
	}

	parsedRemote, err := crd.Unmarshal()
	if err != nil {
		t.params.Logger.Warnw("could not parse remote description", err, "offer", crd)
		return err
	}

	// check if BUNDLE mid matches the "mid" in the SDP fragment
	bundleMid, found := lksdp.GetBundleMid(parsedRemote)
	if !found {
		return ErrNoBundleMid
	}

	if parsedFragment.Mid() != bundleMid {
		t.params.Logger.Warnw("incorrect mid", nil, "sdpFragment", sdpFragment)
		return ErrMidMismatch
	}

	fragmentICEUfrag, fragmentICEPwd, err := parsedFragment.ExtractICECredential()
	if err != nil {
		t.params.Logger.Warnw(
			"could not get ICE crendential from fragment", err,
			"sdpFragment", sdpFragment,
		)
		return ErrInvalidSDPFragment
	}
	remoteICEUfrag, remoteICEPwd, err := lksdp.ExtractICECredential(parsedRemote)
	if err != nil {
		t.params.Logger.Warnw("could not get ICE crendential from remote description", err, "sdpFragment", sdpFragment, "remoteDescription", crd)
		return err
	}
	if fragmentICEUfrag != "" && fragmentICEUfrag != remoteICEUfrag {
		t.params.Logger.Warnw(
			"ice ufrag mismatch", nil,
			"remoteICEUfrag", remoteICEUfrag,
			"fragmentICEUfrag", fragmentICEUfrag,
			"sdpFragment", sdpFragment,
			"remoteDescription", crd,
		)
		return ErrICECredentialMismatch
	}
	if fragmentICEPwd != "" && fragmentICEPwd != remoteICEPwd {
		t.params.Logger.Warnw(
			"ice pwd mismatch", nil,
			"remoteICEPwd", remoteICEPwd,
			"fragmentICEPwd", fragmentICEPwd,
			"sdpFragment", sdpFragment,
			"remoteDescription", crd,
		)
		return ErrICECredentialMismatch
	}

	// add candidates from media description
	for _, ic := range parsedFragment.Candidates() {
		c, err := ice.UnmarshalCandidate(ic)
		if err == nil {
			t.connectionDetails.AddRemoteICECandidate(c, false, false, false)
		}

		candidate := webrtc.ICECandidateInit{
			Candidate: ic,
		}
		if err := t.pc.AddICECandidate(candidate); err != nil {
			t.params.Logger.Warnw("failed to add ICE candidate", err, "candidate", candidate)
		} else {
			t.params.Logger.Debugw("added ICE candidate", "candidate", candidate)
		}
	}
	return nil
}

// Handles SDP Fragment for ICE Restart in WHIP
func (t *PCTransport) HandleICERestartSDPFragment(sdpFragment string) (string, error) {
	if !t.params.UseOneShotSignallingMode {
		return "", ErrNotSynchronousLocalCandidatesMode
	}

	parsedFragment := &lksdp.SDPFragment{}
	if err := parsedFragment.Unmarshal(sdpFragment); err != nil {
		t.params.Logger.Warnw("could not parse SDP fragment", err, "sdpFragment", sdpFragment)
		return "", ErrInvalidSDPFragment
	}

	crd := t.pc.CurrentRemoteDescription()
	if crd == nil {
		t.params.Logger.Warnw("no remote description", nil)
		return "", ErrNoRemoteDescription
	}

	parsedRemote, err := crd.Unmarshal()
	if err != nil {
		t.params.Logger.Warnw("could not parse remote description", err, "offer", crd)
		return "", err
	}

	if err := parsedFragment.PatchICECredentialAndCandidatesIntoSDP(parsedRemote); err != nil {
		t.params.Logger.Warnw("could not patch SDP fragment into remote description", err, "offer", crd, "sdpFragment", sdpFragment)
		return "", err
	}

	bytes, err := parsedRemote.Marshal()
	if err != nil {
		t.params.Logger.Warnw("could not marshal SDP with patched remote", err)
		return "", err
	}
	sd := webrtc.SessionDescription{
		SDP:  string(bytes),
		Type: webrtc.SDPTypeOffer,
	}
	if err := t.pc.SetRemoteDescription(sd); err != nil {
		t.params.Logger.Warnw("could not set remote description", err)
		return "", err
	}

	// clear out connection details on ICE restart and re-populate
	t.connectionDetails.Clear()
	for _, candidate := range parsedFragment.Candidates() {
		c, err := ice.UnmarshalCandidate(candidate)
		if err != nil {
			continue
		}
		t.connectionDetails.AddRemoteICECandidate(c, false, false, false)
	}

	ans, err := t.pc.CreateAnswer(nil)
	if err != nil {
		t.params.Logger.Warnw("could not create answer", err)
		return "", err
	}

	if err = t.pc.SetLocalDescription(ans); err != nil {
		t.params.Logger.Warnw("could not set local description", err)
		return "", err
	}

	// wait for gathering to complete to include all candidates in the answer
	<-webrtc.GatheringCompletePromise(t.pc)

	cld := t.pc.CurrentLocalDescription()

	// add local candidates to ICE connection details
	parsedAnswer, err := cld.Unmarshal()
	if err != nil {
		t.params.Logger.Warnw("could not parse local description", err)
		return "", err
	}

	addLocalICECandidates := func(attrs []sdp.Attribute) {
		for _, a := range attrs {
			if a.IsICECandidate() {
				c, err := ice.UnmarshalCandidate(a.Value)
				if err != nil {
					continue
				}
				t.connectionDetails.AddLocalICECandidate(c, false, false)
			}
		}
	}

	addLocalICECandidates(parsedAnswer.Attributes)
	for _, m := range parsedAnswer.MediaDescriptions {
		addLocalICECandidates(m.Attributes)
	}

	parsedFragmentAnswer, err := lksdp.ExtractSDPFragment(parsedAnswer)
	if err != nil {
		t.params.Logger.Warnw("could not extract SDP fragment", err)
		return "", err
	}

	answerFragment, err := parsedFragmentAnswer.Marshal()
	if err != nil {
		t.params.Logger.Warnw("could not marshal answer SDP fragment", err)
		return "", err
	}

	return answerFragment, nil
}

func (t *PCTransport) OnNegotiationStateChanged(f func(state transport.NegotiationState)) {
	t.lock.Lock()
	t.onNegotiationStateChanged = f
	t.lock.Unlock()
}

func (t *PCTransport) getOnNegotiationStateChanged() func(state transport.NegotiationState) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.onNegotiationStateChanged
}

func (t *PCTransport) Negotiate(force bool) {
	if t.isClosed.Load() {
		return
	}

	var postEvent bool
	t.lock.Lock()
	if force {
		t.debouncedNegotiate.Add(func() {
			// no op to cancel pending negotiation
		})
		t.debouncePending = false
		t.updateLastNegotiateLocked()

		postEvent = true
	} else {
		if !t.debouncePending {
			if time.Since(t.lastNegotiate) > negotiationFrequency {
				t.debouncedNegotiate.SetDuration(fastNegotiationFrequency)
			} else {
				t.debouncedNegotiate.SetDuration(negotiationFrequency)
			}

			t.debouncedNegotiate.Add(func() {
				t.lock.Lock()
				t.debouncePending = false
				t.updateLastNegotiateLocked()
				t.lock.Unlock()

				t.postEvent(event{
					signal: signalSendOffer,
				})
			})
			t.debouncePending = true
		}
	}
	t.lock.Unlock()

	if postEvent {
		t.postEvent(event{
			signal: signalSendOffer,
		})
	}
}

func (t *PCTransport) updateLastNegotiateLocked() {
	if now := time.Now(); now.After(t.lastNegotiate) {
		t.lastNegotiate = now
	}
}

func (t *PCTransport) ICERestart() error {
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		t.params.Logger.Warnw("trying to restart ICE on closed peer connection", nil)
		return ErrIceRestartOnClosedPeerConnection
	}

	t.postEvent(event{
		signal: signalICERestart,
	})
	return nil
}

func (t *PCTransport) ResetShortConnOnICERestart() {
	t.resetShortConnOnICERestart.Store(true)
}

func (t *PCTransport) AddTrackToStreamAllocator(subTrack types.SubscribedTrack) {
	if t.streamAllocator == nil {
		return
	}

	layers := buffer.GetVideoLayersForMimeType(
		subTrack.DownTrack().Mime(),
		subTrack.MediaTrack().ToProto(),
	)
	t.streamAllocator.AddTrack(subTrack.DownTrack(), streamallocator.AddTrackParams{
		Source:         subTrack.MediaTrack().Source(),
		IsMultiLayered: len(layers) > 1,
		PublisherID:    subTrack.MediaTrack().PublisherID(),
	})
}

func (t *PCTransport) RemoveTrackFromStreamAllocator(subTrack types.SubscribedTrack) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.RemoveTrack(subTrack.DownTrack())
}

func (t *PCTransport) SetAllowPauseOfStreamAllocator(allowPause bool) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.SetAllowPause(allowPause)
}

func (t *PCTransport) SetChannelCapacityOfStreamAllocator(channelCapacity int64) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.SetChannelCapacity(channelCapacity)
}

func (t *PCTransport) preparePC(previousAnswer webrtc.SessionDescription) error {
	// sticky data channel to first m-lines, if someday we don't send sdp without media streams to
	// client's subscribe pc after joining, should change this step
	parsed, err := previousAnswer.Unmarshal()
	if err != nil {
		return err
	}
	fp, fpHahs, err := lksdp.ExtractFingerprint(parsed)
	if err != nil {
		return err
	}

	offer, err := t.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := t.pc.SetLocalDescription(offer); err != nil {
		return err
	}

	//
	// Simulate client side peer connection and set DTLS role from previous answer.
	// Role needs to be set properly (one side needs to be server and the other side
	// needs to be the client) for DTLS connection to form properly. As this is
	// trying to replicate previous setup, read from previous answer and use that role.
	//
	se := webrtc.SettingEngine{}
	_ = se.SetAnsweringDTLSRole(lksdp.ExtractDTLSRole(parsed))
	api := webrtc.NewAPI(
		webrtc.WithSettingEngine(se),
		webrtc.WithMediaEngine(t.me),
	)
	pc2, err := api.NewPeerConnection(webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		return err
	}
	defer pc2.Close()

	if err := pc2.SetRemoteDescription(offer); err != nil {
		return err
	}
	ans, err := pc2.CreateAnswer(nil)
	if err != nil {
		return err
	}

	// replace client's fingerprint into dummy pc's answer, for pion's dtls process, it will
	// keep the fingerprint at first call of SetRemoteDescription, if dummy pc and client pc use
	// different fingerprint, that will cause pion denied dtls data after handshake with client
	// complete (can't pass fingerprint change).
	// in this step, we don't established connection with dummy pc(no candidate swap), just use
	// sdp negotiation to sticky data channel and keep client's fingerprint
	parsedAns, _ := ans.Unmarshal()
	fpLine := fpHahs + " " + fp
	replaceFP := func(attrs []sdp.Attribute, fpLine string) {
		for k := range attrs {
			if attrs[k].Key == "fingerprint" {
				attrs[k].Value = fpLine
			}
		}
	}
	replaceFP(parsedAns.Attributes, fpLine)
	for _, m := range parsedAns.MediaDescriptions {
		replaceFP(m.Attributes, fpLine)
	}
	bytes, err := parsedAns.Marshal()
	if err != nil {
		return err
	}
	ans.SDP = string(bytes)

	return t.pc.SetRemoteDescription(ans)
}

func (t *PCTransport) initPCWithPreviousAnswer(previousAnswer webrtc.SessionDescription) (map[string]*webrtc.RTPSender, error) {
	senders := make(map[string]*webrtc.RTPSender)
	parsed, err := previousAnswer.Unmarshal()
	if err != nil {
		return senders, err
	}
	for _, m := range parsed.MediaDescriptions {
		var codecType webrtc.RTPCodecType
		switch m.MediaName.Media {
		case "video":
			codecType = webrtc.RTPCodecTypeVideo
		case "audio":
			codecType = webrtc.RTPCodecTypeAudio
		case "application":
			if t.params.IsOfferer {
				// for pion generate unmatched sdp, it always appends data channel to last m-lines,
				// that not consistent with our previous answer that data channel might at middle-line
				// because sdp can negotiate multi times before migration.(it will sticky to the last m-line at first negotiate)
				// so use a dummy pc to negotiate sdp to fixed the datachannel's mid at same position with previous answer
				if err := t.preparePC(previousAnswer); err != nil {
					t.params.Logger.Warnw("prepare pc for migration failed", err)
					return senders, err
				}
			}
			continue
		default:
			continue
		}

		if !t.params.IsOfferer {
			// `sendrecv` or `sendonly` means this transceiver is used for sending

			// Note that a transceiver previously used to send could be `inactive`.
			// Let those transceivers be created when remote description is set.
			_, ok1 := m.Attribute(webrtc.RTPTransceiverDirectionSendrecv.String())
			_, ok2 := m.Attribute(webrtc.RTPTransceiverDirectionSendonly.String())
			if !ok1 && !ok2 {
				continue
			}
		}

		tr, err := t.pc.AddTransceiverFromKind(
			codecType,
			webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendonly,
			},
		)
		if err != nil {
			return senders, err
		}
		mid := lksdp.GetMidValue(m)
		if mid == "" {
			return senders, ErrMidNotFound
		}
		tr.SetMid(mid)

		// save mid -> senders for migration reuse
		sender := tr.Sender()
		senders[mid] = sender

		// set transceiver to inactive
		tr.SetSender(sender, nil)
	}
	return senders, nil
}

func (t *PCTransport) SetPreviousSdp(localDescription, remoteDescription *webrtc.SessionDescription) {
	// when there is no answer, cannot migrate, force a full reconnect
	if (t.params.IsOfferer && remoteDescription == nil) || (!t.params.IsOfferer && localDescription == nil) {
		t.onNegotiationFailed(true, "no previous answer")
		return
	}

	t.lock.Lock()
	var (
		senders   map[string]*webrtc.RTPSender
		err       error
		parseMids bool
	)
	if t.params.IsOfferer {
		if t.pc.RemoteDescription() == nil && t.previousAnswer == nil {
			t.previousAnswer = remoteDescription
			senders, err = t.initPCWithPreviousAnswer(*remoteDescription)
			parseMids = true
		}
	} else {
		if t.pc.LocalDescription() == nil {
			senders, err = t.initPCWithPreviousAnswer(*localDescription)
			parseMids = true
		}
	}
	if err != nil {
		t.lock.Unlock()
		t.onNegotiationFailed(true, fmt.Sprintf("initPCWithPreviousAnswer failed, error: %s", err))
		return
	}

	if localDescription != nil && parseMids {
		// in migration case, can't reuse transceiver before negotiating excepted tracks
		// that were subscribed at previous node
		t.canReuseTransceiver = false
		if err := t.parseTrackMid(*localDescription, senders); err != nil {
			t.params.Logger.Warnw(
				"parse previous local description failed", err,
				"localDescription", localDescription.SDP,
			)
		}
	}

	if t.params.IsOfferer {
		// disable fast negotiation temporarily after migration to avoid sending offer
		// contains part of subscribed tracks before migration, let the subscribed track
		// resume at the same time.
		t.lastNegotiate = time.Now().Add(iceFailedTimeoutTotal)
	}
	t.lock.Unlock()
}

func (t *PCTransport) parseTrackMid(sd webrtc.SessionDescription, senders map[string]*webrtc.RTPSender) error {
	parsed, err := sd.Unmarshal()
	if err != nil {
		return err
	}

	t.previousTrackDescription = make(map[string]*trackDescription)
	for _, m := range parsed.MediaDescriptions {
		msid, ok := m.Attribute(sdp.AttrKeyMsid)
		if !ok {
			continue
		}

		if split := strings.Split(msid, " "); len(split) == 2 {
			trackID := split[1]
			mid := lksdp.GetMidValue(m)
			if mid == "" {
				return ErrMidNotFound
			}
			if sender, ok := senders[mid]; ok {
				t.previousTrackDescription[trackID] = &trackDescription{mid, sender}
			}
		}
	}
	return nil
}

func (t *PCTransport) postEvent(e event) {
	e.PCTransport = t
	t.eventsQueue.Enqueue(func(e event) {
		var err error
		switch e.signal {
		case signalICEGatheringComplete:
			err = e.handleICEGatheringComplete(e)
		case signalLocalICECandidate:
			err = e.handleLocalICECandidate(e)
		case signalRemoteICECandidate:
			err = e.handleRemoteICECandidate(e)
		case signalSendOffer:
			err = e.handleSendOffer(e)
		case signalRemoteDescriptionReceived:
			err = e.handleRemoteDescriptionReceived(e)
		case signalICERestart:
			err = e.handleICERestart(e)
		}
		if err != nil {
			if !e.isClosed.Load() {
				e.onNegotiationFailed(true, fmt.Sprintf("error handling event. err: %s, event: %s", err, e))
			}
		}
	}, e)
}

func (t *PCTransport) handleICEGatheringComplete(_ event) error {
	if t.params.IsOfferer {
		return t.handleICEGatheringCompleteOfferer()
	} else {
		return t.handleICEGatheringCompleteAnswerer()
	}
}

func (t *PCTransport) handleICEGatheringCompleteOfferer() error {
	if !t.restartAfterGathering {
		return nil
	}

	t.params.Logger.Debugw("restarting ICE after ICE gathering")
	t.restartAfterGathering = false
	return t.doICERestart()
}

func (t *PCTransport) handleICEGatheringCompleteAnswerer() error {
	if t.pendingRestartIceOffer == nil {
		return nil
	}

	offer := *t.pendingRestartIceOffer
	t.pendingRestartIceOffer = nil

	t.params.Logger.Debugw("accept remote restart ice offer after ICE gathering")
	if err := t.setRemoteDescription(offer); err != nil {
		return err
	}
	t.params.Handler.OnSetRemoteDescriptionOffer()
	t.processSendersPendingConfig()

	return t.createAndSendAnswer()
}

func (t *PCTransport) localDescriptionSent() error {
	if !t.cacheLocalCandidates {
		return nil
	}

	t.cacheLocalCandidates = false

	cachedLocalCandidates := t.cachedLocalCandidates
	t.cachedLocalCandidates = nil

	for _, c := range cachedLocalCandidates {
		if err := t.params.Handler.OnICECandidate(c, t.params.Transport); err != nil {
			t.params.Logger.Warnw("failed to send cached ICE candidate", err, "candidate", c)
			return err
		}
	}
	return nil
}

func (t *PCTransport) clearLocalDescriptionSent() {
	t.cacheLocalCandidates = true
	t.cachedLocalCandidates = nil
	t.connectionDetails.Clear()
}

func (t *PCTransport) handleLocalICECandidate(e event) error {
	c := e.data.(*webrtc.ICECandidate)

	filtered := false
	if c != nil {
		if t.preferTCP.Load() && c.Protocol != webrtc.ICEProtocolTCP {
			t.params.Logger.Debugw("filtering out local candidate", "candidate", c.String())
			filtered = true
		}
		t.connectionDetails.AddLocalCandidate(c, filtered, true)
	}

	if filtered {
		return nil
	}

	if t.cacheLocalCandidates {
		t.cachedLocalCandidates = append(t.cachedLocalCandidates, c)
		return nil
	}

	if err := t.params.Handler.OnICECandidate(c, t.params.Transport); err != nil {
		t.params.Logger.Warnw("failed to send ICE candidate", err, "candidate", c)
		return err
	}

	return nil
}

func (t *PCTransport) handleRemoteICECandidate(e event) error {
	c := e.data.(*webrtc.ICECandidateInit)

	filtered := false
	if t.preferTCP.Load() && !strings.Contains(strings.ToLower(c.Candidate), "tcp") {
		t.params.Logger.Debugw("filtering out remote candidate", "candidate", c.Candidate)
		filtered = true
	}

	if !t.params.Config.UseMDNS && types.IsCandidateMDNS(*c) {
		t.params.Logger.Debugw("ignoring mDNS candidate", "candidate", c.Candidate)
		filtered = true
	}

	t.connectionDetails.AddRemoteCandidate(*c, filtered, true, false)
	if filtered {
		return nil
	}

	if t.pc.RemoteDescription() == nil {
		t.pendingRemoteCandidates = append(t.pendingRemoteCandidates, c)
		return nil
	}

	if err := t.pc.AddICECandidate(*c); err != nil {
		t.params.Logger.Warnw("failed to add ICE candidate", err, "candidate", c)
		return errors.Wrap(err, "add ice candidate failed")
	} else {
		t.params.Logger.Debugw("added ICE candidate", "candidate", c)
	}

	return nil
}

func (t *PCTransport) setNegotiationState(state transport.NegotiationState) {
	t.negotiationState = state
	if onNegotiationStateChanged := t.getOnNegotiationStateChanged(); onNegotiationStateChanged != nil {
		onNegotiationStateChanged(t.negotiationState)
	}
}

func (t *PCTransport) filterCandidates(sd webrtc.SessionDescription, preferTCP, isLocal bool) webrtc.SessionDescription {
	parsed, err := sd.Unmarshal()
	if err != nil {
		t.params.Logger.Warnw("could not unmarshal SDP to filter candidates", err)
		return sd
	}

	filterAttributes := func(attrs []sdp.Attribute) []sdp.Attribute {
		filteredAttrs := make([]sdp.Attribute, 0, len(attrs))
		for _, a := range attrs {
			if a.IsICECandidate() {
				c, err := ice.UnmarshalCandidate(a.Value)
				if err != nil {
					t.params.Logger.Errorw("failed to unmarshal candidate in sdp", err, "isLocal", isLocal, "sdp", sd.SDP)
					filteredAttrs = append(filteredAttrs, a)
					continue
				}
				excluded := preferTCP && !c.NetworkType().IsTCP()
				if !excluded {
					if !t.params.Config.UseMDNS && types.IsICECandidateMDNS(c) {
						excluded = true
					}
				}
				if !excluded {
					filteredAttrs = append(filteredAttrs, a)
				}

				if isLocal {
					t.connectionDetails.AddLocalICECandidate(c, excluded, false)
				} else {
					t.connectionDetails.AddRemoteICECandidate(c, excluded, false, false)
				}
			} else {
				filteredAttrs = append(filteredAttrs, a)
			}
		}

		return filteredAttrs
	}

	parsed.Attributes = filterAttributes(parsed.Attributes)
	for _, m := range parsed.MediaDescriptions {
		m.Attributes = filterAttributes(m.Attributes)
	}

	bytes, err := parsed.Marshal()
	if err != nil {
		t.params.Logger.Warnw("could not marshal SDP to filter candidates", err)
		return sd
	}
	sd.SDP = string(bytes)
	return sd
}

func (t *PCTransport) clearSignalStateCheckTimer() {
	if t.signalStateCheckTimer != nil {
		t.signalStateCheckTimer.Stop()
		t.signalStateCheckTimer = nil
	}
}

func (t *PCTransport) setupSignalStateCheckTimer() {
	t.clearSignalStateCheckTimer()

	negotiateVersion := t.negotiateCounter.Inc()
	t.signalStateCheckTimer = time.AfterFunc(negotiationFailedTimeout, func() {
		t.clearSignalStateCheckTimer()

		failed := t.negotiationState != transport.NegotiationStateNone

		if t.negotiateCounter.Load() == negotiateVersion && failed && t.pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
			t.onNegotiationFailed(false, "negotiation timed out")
		}
	})
}

func (t *PCTransport) adjustNumOutstandingMedia(transceiver *webrtc.RTPTransceiver) {
	if transceiver.Mid() != "" {
		return
	}

	t.lock.Lock()
	if transceiver.Kind() == webrtc.RTPCodecTypeAudio {
		t.numOutstandingAudios++
	} else {
		t.numOutstandingVideos++
	}
	t.lock.Unlock()
}

func (t *PCTransport) sendUnmatchedMediaRequirement(force bool) error {
	// if there are unmatched media sections, notify remote peer to generate offer with
	// enough media section in subsequent offers
	t.lock.Lock()
	numAudios := t.numOutstandingAudios - t.numRequestSentAudios
	t.numRequestSentAudios += numAudios

	numVideos := t.numOutstandingVideos - t.numRequestSentVideos
	t.numRequestSentVideos += numVideos
	t.lock.Unlock()

	if force || (numAudios+numVideos) != 0 {
		if err := t.params.Handler.OnUnmatchedMedia(numAudios, numVideos); err != nil {
			return errors.Wrap(err, "could not send unmatched media requirements")
		}
	}

	return nil
}

func (t *PCTransport) createAndSendOffer(options *webrtc.OfferOptions) error {
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		t.params.Logger.Warnw("trying to send offer on closed peer connection", nil)
		return nil
	}

	// when there's an ongoing negotiation, let it finish and not disrupt its state
	if t.negotiationState == transport.NegotiationStateRemote {
		t.params.Logger.Debugw("skipping negotiation, trying again later")
		t.setNegotiationState(transport.NegotiationStateRetry)
		return nil
	} else if t.negotiationState == transport.NegotiationStateRetry {
		// already set to retry, we can safely skip this attempt
		return nil
	}

	ensureICERestart := func(options *webrtc.OfferOptions) *webrtc.OfferOptions {
		if options == nil {
			options = &webrtc.OfferOptions{}
		}
		options.ICERestart = true
		return options
	}

	t.lock.Lock()
	if t.previousAnswer != nil {
		t.previousAnswer = nil
		options = ensureICERestart(options)
		t.params.Logger.Infow("ice restart due to previous answer")
	}
	t.lock.Unlock()

	if t.restartAtNextOffer {
		t.restartAtNextOffer = false
		options = ensureICERestart(options)
		t.params.Logger.Infow("ice restart at next offer")
	}

	if options != nil && options.ICERestart {
		t.clearLocalDescriptionSent()
	}

	offer, err := t.pc.CreateOffer(options)
	if err != nil {
		if errors.Is(err, webrtc.ErrConnectionClosed) {
			t.params.Logger.Warnw("trying to create offer on closed peer connection", nil)
			return nil
		}

		prometheus.RecordServiceOperationError("offer", "create")
		return errors.Wrap(err, "create offer failed")
	}

	preferTCP := t.preferTCP.Load()
	if preferTCP {
		t.params.Logger.Debugw("local offer (unfiltered)", "sdp", offer.SDP)
	}

	err = t.pc.SetLocalDescription(offer)
	if err != nil {
		if errors.Is(err, webrtc.ErrConnectionClosed) {
			t.params.Logger.Warnw("trying to set local description on closed peer connection", nil)
			return nil
		}

		prometheus.RecordServiceOperationError("offer", "local_description")
		return errors.Wrap(err, "setting local description failed")
	}

	//
	// Filter after setting local description as pion expects the offer
	// to match between CreateOffer and SetLocalDescription.
	// Filtered offer is sent to remote so that remote does not
	// see filtered candidates.
	//
	offer = t.filterCandidates(offer, preferTCP, true)
	if preferTCP {
		t.params.Logger.Debugw("local offer (filtered)", "sdp", offer.SDP)
	}

	// indicate waiting for remote
	t.setNegotiationState(transport.NegotiationStateRemote)

	t.setupSignalStateCheckTimer()

	remoteAnswerId := t.remoteAnswerId.Load()
	if remoteAnswerId != 0 && remoteAnswerId != t.localOfferId.Load() {
		t.params.Logger.Warnw(
			"sdp state: sending offer before receiving answer", nil,
			"localOfferId", t.localOfferId.Load(),
			"remoteAnswerId", remoteAnswerId,
		)
	}

	if err := t.params.Handler.OnOffer(offer, t.localOfferId.Inc()); err != nil {
		prometheus.RecordServiceOperationError("offer", "write_message")
		return errors.Wrap(err, "could not send offer")
	}
	prometheus.RecordServiceOperationSuccess("offer")

	return t.localDescriptionSent()
}

func (t *PCTransport) handleSendOffer(_ event) error {
	if !t.params.IsOfferer {
		return t.sendUnmatchedMediaRequirement(true)
	}

	return t.createAndSendOffer(nil)
}

type remoteDescriptionData struct {
	sessionDescription *webrtc.SessionDescription
	remoteId           uint32
}

func (t *PCTransport) handleRemoteDescriptionReceived(e event) error {
	rdd := e.data.(remoteDescriptionData)
	if rdd.sessionDescription.Type == webrtc.SDPTypeOffer {
		return t.handleRemoteOfferReceived(rdd.sessionDescription, rdd.remoteId)
	} else {
		return t.handleRemoteAnswerReceived(rdd.sessionDescription, rdd.remoteId)
	}
}

func (t *PCTransport) isRemoteOfferRestartICE(parsed *sdp.SessionDescription) (string, bool, error) {
	user, pwd, err := lksdp.ExtractICECredential(parsed)
	if err != nil {
		return "", false, err
	}

	credential := fmt.Sprintf("%s:%s", user, pwd)
	// ice credential changed, remote offer restart ice
	restartICE := t.currentOfferIceCredential != "" && t.currentOfferIceCredential != credential
	return credential, restartICE, nil
}

func (t *PCTransport) setRemoteDescription(sd webrtc.SessionDescription) error {
	// filter before setting remote description so that pion does not see filtered remote candidates
	preferTCP := t.preferTCP.Load()
	if preferTCP {
		t.params.Logger.Debugw("remote description (unfiltered)", "type", sd.Type, "sdp", sd.SDP)
	}
	sd = t.filterCandidates(sd, preferTCP, false)
	if preferTCP {
		t.params.Logger.Debugw("remote description (filtered)", "type", sd.Type, "sdp", sd.SDP)
	}

	if err := t.pc.SetRemoteDescription(sd); err != nil {
		if errors.Is(err, webrtc.ErrConnectionClosed) {
			t.params.Logger.Warnw("trying to set remote description on closed peer connection", nil)
			return nil
		}

		sdpType := "offer"
		if sd.Type == webrtc.SDPTypeAnswer {
			sdpType = "answer"
		}
		prometheus.RecordServiceOperationError(sdpType, "remote_description")
		return errors.Wrap(err, "setting remote description failed")
	} else if sd.Type == webrtc.SDPTypeAnswer {
		t.lock.Lock()
		if !t.canReuseTransceiver {
			t.canReuseTransceiver = true
			t.previousTrackDescription = make(map[string]*trackDescription)
		}
		t.lock.Unlock()
	}

	for _, c := range t.pendingRemoteCandidates {
		if err := t.pc.AddICECandidate(*c); err != nil {
			t.params.Logger.Warnw("failed to add cached ICE candidate", err, "candidate", c)
			return errors.Wrap(err, "add ice candidate failed")
		} else {
			t.params.Logger.Debugw("added cached ICE candidate", "candidate", c)
		}
	}
	t.pendingRemoteCandidates = nil

	return nil
}

func (t *PCTransport) createAndSendAnswer() error {
	numOutstandingAudios, numOutstandingVideos := t.getNumUnmatchedTransceivers()
	t.lock.Lock()
	t.numOutstandingAudios, t.numOutstandingVideos = numOutstandingAudios, numOutstandingVideos
	t.numRequestSentAudios, t.numRequestSentVideos = 0, 0
	t.lock.Unlock()

	answer, err := t.pc.CreateAnswer(nil)
	if err != nil {
		if errors.Is(err, webrtc.ErrConnectionClosed) {
			t.params.Logger.Warnw("trying to create answer on closed peer connection", nil)
			return nil
		}

		prometheus.RecordServiceOperationError("answer", "create")
		return errors.Wrap(err, "create answer failed")
	}

	preferTCP := t.preferTCP.Load()
	if preferTCP {
		t.params.Logger.Debugw("local answer (unfiltered)", "sdp", answer.SDP)
	}

	if err = t.pc.SetLocalDescription(answer); err != nil {
		prometheus.RecordServiceOperationError("answer", "local_description")
		return errors.Wrap(err, "setting local description failed")
	}

	midToTrackID := map[string]string{}
	for _, tr := range t.pc.GetTransceivers() {
		if tr.Sender() != nil && tr.Mid() != "" {
			midToTrackID[tr.Mid()] = tr.Sender().Track().ID()
		}
	}

	//
	// Filter after setting local description as pion expects the answer
	// to match between CreateAnswer and SetLocalDescription.
	// Filtered answer is sent to remote so that remote does not
	// see filtered candidates.
	//
	answer = t.filterCandidates(answer, preferTCP, true)
	if preferTCP {
		t.params.Logger.Debugw("local answer (filtered)", "sdp", answer.SDP)
	}

	localAnswerId := t.localAnswerId.Load()
	if localAnswerId != 0 && localAnswerId >= t.remoteOfferId.Load() {
		t.params.Logger.Warnw(
			"sdp state: duplicate answer", nil,
			"localAnswerId", localAnswerId,
			"remoteOfferId", t.remoteOfferId.Load(),
		)
	}

	answerId := t.remoteOfferId.Load()
	if err := t.params.Handler.OnAnswer(answer, answerId, midToTrackID); err != nil {
		prometheus.RecordServiceOperationError("answer", "write_message")
		return errors.Wrap(err, "could not send answer")
	}
	t.localAnswerId.Store(answerId)
	prometheus.RecordServiceOperationSuccess("asnwer")

	if err := t.sendUnmatchedMediaRequirement(false); err != nil {
		return err
	}

	t.lock.Lock()
	if !t.canReuseTransceiver {
		t.canReuseTransceiver = true
		t.previousTrackDescription = make(map[string]*trackDescription)
	}
	t.lock.Unlock()

	return t.localDescriptionSent()
}

func (t *PCTransport) handleRemoteOfferReceived(sd *webrtc.SessionDescription, offerId uint32) error {
	t.params.Logger.Debugw("processing offer", "offerId", offerId)
	remoteOfferId := t.remoteOfferId.Load()
	if remoteOfferId != 0 && remoteOfferId != t.localAnswerId.Load() {
		t.params.Logger.Warnw(
			"sdp state: multiple offers without answer", nil,
			"remoteOfferId", remoteOfferId,
			"localAnswerId", t.localAnswerId.Load(),
			"receivedRemoteOfferId", offerId,
		)
	}
	t.remoteOfferId.Store(offerId)

	parsed, err := sd.Unmarshal()
	if err != nil {
		return err
	}

	t.lock.Lock()
	if !t.firstOfferReceived {
		t.firstOfferReceived = true
		var dataChannelFound bool
		for _, media := range parsed.MediaDescriptions {
			if strings.EqualFold(media.MediaName.Media, "application") {
				dataChannelFound = true
				break
			}
		}
		t.firstOfferNoDataChannel = !dataChannelFound
	}
	t.lock.Unlock()

	iceCredential, offerRestartICE, err := t.isRemoteOfferRestartICE(parsed)
	if err != nil {
		return errors.Wrap(err, "check remote offer restart ice failed")
	}

	if offerRestartICE && t.pendingRestartIceOffer == nil {
		t.clearLocalDescriptionSent()
	}

	if offerRestartICE && t.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
		t.params.Logger.Debugw("remote offer restart ice while ice gathering")
		t.pendingRestartIceOffer = sd
		return nil
	}

	if offerRestartICE && t.resetShortConnOnICERestart.CompareAndSwap(true, false) {
		t.resetShortConn()
	}

	if offerRestartICE {
		t.outputAndClearICEStats()
	}

	if err := t.setRemoteDescription(*sd); err != nil {
		return err
	}
	t.params.Handler.OnSetRemoteDescriptionOffer()
	t.processSendersPendingConfig()

	rtxRepairs := nonSimulcastRTXRepairsFromSDP(parsed, t.params.Logger)
	if len(rtxRepairs) > 0 {
		t.params.Logger.Debugw("rtx pairs found from sdp", "ssrcs", rtxRepairs)
		for repair, base := range rtxRepairs {
			t.params.Config.BufferFactory.SetRTXPair(repair, base)
		}
	}

	if t.currentOfferIceCredential == "" || offerRestartICE {
		t.currentOfferIceCredential = iceCredential
	}

	return t.createAndSendAnswer()
}

func (t *PCTransport) handleRemoteAnswerReceived(sd *webrtc.SessionDescription, answerId uint32) error {
	t.params.Logger.Debugw("processing answer", "answerId", answerId)
	if answerId != 0 && answerId != t.localOfferId.Load() {
		t.params.Logger.Warnw(
			"sdp state: answer id mismatch", nil,
			"expected", t.localOfferId.Load(),
			"got", answerId,
		)
	}
	t.remoteAnswerId.Store(answerId)

	t.clearSignalStateCheckTimer()

	if err := t.setRemoteDescription(*sd); err != nil {
		// Pion will call RTPSender.Send method for each new added Downtrack, and return error if the DownTrack.Bind
		// returns error. In case of Downtrack.Bind returns ErrUnsupportedCodec, the signal state will be stable as negotiation is aleady compelted
		// before startRTPSenders, and the peerconnection state can be recovered by next negotiation which will be triggered
		// by the SubscriptionManager unsubscribe the failure DownTrack. So don't treat this error as negotiation failure.
		if !errors.Is(err, webrtc.ErrUnsupportedCodec) {
			return err
		}
	}

	if t.negotiationState == transport.NegotiationStateRetry {
		t.setNegotiationState(transport.NegotiationStateNone)

		t.params.Logger.Debugw("re-negotiate after receiving answer")
		return t.createAndSendOffer(nil)
	}

	t.setNegotiationState(transport.NegotiationStateNone)
	return nil
}

func (t *PCTransport) doICERestart() error {
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		t.params.Logger.Warnw("trying to restart ICE on closed peer connection", nil)
		return nil
	}

	// if restart is requested, but negotiation never started
	iceGatheringState := t.pc.ICEGatheringState()
	if iceGatheringState == webrtc.ICEGatheringStateNew {
		t.params.Logger.Debugw("skipping ICE restart on not yet started peer connection")
		return nil
	}

	// if restart is requested, and we are not ready, then continue afterwards
	if iceGatheringState == webrtc.ICEGatheringStateGathering {
		t.params.Logger.Debugw("deferring ICE restart to after gathering")
		t.restartAfterGathering = true
		return nil
	}

	if t.resetShortConnOnICERestart.CompareAndSwap(true, false) {
		t.resetShortConn()
	}

	if t.negotiationState == transport.NegotiationStateNone {
		t.outputAndClearICEStats()
		return t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true})
	}

	currentRemoteDescription := t.pc.CurrentRemoteDescription()
	if currentRemoteDescription == nil {
		// restart without current remote description, send current local description again to try recover
		offer := t.pc.LocalDescription()
		if offer == nil {
			// it should not happen, log just in case
			t.params.Logger.Warnw("ice restart without local offer", nil)
			return ErrIceRestartWithoutLocalSDP
		} else {
			t.params.Logger.Infow("deferring ice restart to next offer")
			t.setNegotiationState(transport.NegotiationStateRetry)
			t.restartAtNextOffer = true

			remoteAnswerId := t.remoteAnswerId.Load()
			if remoteAnswerId != 0 && remoteAnswerId != t.localOfferId.Load() {
				t.params.Logger.Warnw(
					"sdp state: answer not received in ICE restart", nil,
					"localOfferId", t.localOfferId.Load(),
					"remoteAnswerId", remoteAnswerId,
				)
			}

			err := t.params.Handler.OnOffer(*offer, t.localOfferId.Inc())
			if err != nil {
				prometheus.RecordServiceOperationError("offer", "write_message")
			} else {
				prometheus.RecordServiceOperationSuccess("offer")
			}
			return err
		}
	} else {
		// recover by re-applying the last answer
		t.params.Logger.Infow("recovering from client negotiation state on ICE restart")
		if err := t.pc.SetRemoteDescription(*currentRemoteDescription); err != nil {
			prometheus.RecordServiceOperationError("offer", "remote_description")
			return errors.Wrap(err, "set remote description failed")
		} else {
			t.setNegotiationState(transport.NegotiationStateNone)
			t.outputAndClearICEStats()
			return t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true})
		}
	}
}

func (t *PCTransport) handleICERestart(_ event) error {
	return t.doICERestart()
}

func (t *PCTransport) onNegotiationFailed(warning bool, reason string) {
	logFields := []interface{}{
		"reason", reason,
		"localCurrent", t.pc.CurrentLocalDescription(),
		"localPending", t.pc.PendingLocalDescription(),
		"remoteCurrent", t.pc.CurrentRemoteDescription(),
		"remotePending", t.pc.PendingRemoteDescription(),
	}
	if warning {
		t.params.Logger.Warnw(
			"negotiation failed",
			nil,
			logFields...,
		)
	} else {
		t.params.Logger.Infow("negotiation failed", logFields...)
	}
	t.params.Handler.OnNegotiationFailed()
}

func (t *PCTransport) outputAndClearICEStats() {
	t.lock.Lock()
	stats := t.mayFailedICEStats
	t.mayFailedICEStats = nil
	t.lock.Unlock()

	if len(stats) > 0 {
		t.params.Logger.Infow("ICE candidate pair stats", "stats", iceCandidatePairStatsEncoder{stats})
	}
}

// ----------------------

type configureSenderParams struct {
	transceiver              *webrtc.RTPTransceiver
	enabledCodecs            []*livekit.Codec
	rtcpFeedbackConfig       RTCPFeedbackConfig
	filterOutH264HighProfile bool
	enableAudioStereo        bool
	enableAudioNACK          bool
}

func configureSender(params configureSenderParams) {
	configureSenderCodecs(
		params.transceiver,
		params.enabledCodecs,
		params.rtcpFeedbackConfig,
		params.filterOutH264HighProfile,
	)

	if params.transceiver.Kind() == webrtc.RTPCodecTypeAudio {
		configureSenderAudio(params.transceiver, params.enableAudioStereo, params.enableAudioNACK)
	}
}

// configure subscriber transceiver for audio stereo and nack
// pion doesn't support per transciver codec configuration, so the nack of this session will be disabled
// forever once it is first disabled by a transceiver.
func configureSenderAudio(tr *webrtc.RTPTransceiver, stereo bool, nack bool) {
	sender := tr.Sender()
	if sender == nil {
		return
	}

	// enable stereo
	codecs := sender.GetParameters().Codecs
	configCodecs := make([]webrtc.RTPCodecParameters, 0, len(codecs))
	for _, c := range codecs {
		if mime.IsMimeTypeStringOpus(c.MimeType) {
			c.SDPFmtpLine = strings.ReplaceAll(c.SDPFmtpLine, ";sprop-stereo=1", "")
			if stereo {
				c.SDPFmtpLine += ";sprop-stereo=1"
			}
			if !nack {
				for i, fb := range c.RTCPFeedback {
					if fb.Type == webrtc.TypeRTCPFBNACK {
						c.RTCPFeedback = append(c.RTCPFeedback[:i], c.RTCPFeedback[i+1:]...)
						break
					}
				}
			}
		}
		configCodecs = append(configCodecs, c)
	}

	tr.SetCodecPreferences(configCodecs)
}

// In single peer connection mode, set up enebled codecs for sender.
// The config provides config of direction.
// For publisher peer connection those are publish enabled codecs
// and for subscriber peer connection those are subscribe enabled codecs.
//
// But, in single peer connection mode, if setting up a transceiver where the media is
// flowing in the other direction, the other direction codec config needs to be set.
func configureSenderCodecs(
	tr *webrtc.RTPTransceiver,
	enabledCodecs []*livekit.Codec,
	rtcpFeedbackConfig RTCPFeedbackConfig,
	filterOutH264HighProfile bool,
) {
	if len(enabledCodecs) == 0 {
		return
	}

	sender := tr.Sender()
	if sender == nil {
		return
	}

	filteredCodecs := filterCodecs(
		sender.GetParameters().Codecs,
		enabledCodecs,
		rtcpFeedbackConfig,
		filterOutH264HighProfile,
	)
	tr.SetCodecPreferences(filteredCodecs)
}

func configureReceiverCodecs(
	tr *webrtc.RTPTransceiver,
	preferredMimeType string,
	compliesWithCodecOrderInSDPAnswer bool,
) {
	receiver := tr.Receiver()
	if receiver == nil {
		return
	}

	var preferredCodecs, leftCodecs []webrtc.RTPCodecParameters
	for _, c := range receiver.GetParameters().Codecs {
		if tr.Kind() == webrtc.RTPCodecTypeAudio {
			nackFound := false
			for _, fb := range c.RTCPFeedback {
				if fb.Type == webrtc.TypeRTCPFBNACK {
					nackFound = true
					break
				}
			}

			if !nackFound {
				c.RTCPFeedback = append(c.RTCPFeedback, webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBNACK})
			}
		}

		if mime.GetMimeTypeCodec(preferredMimeType) == mime.GetMimeTypeCodec(c.RTPCodecCapability.MimeType) {
			preferredCodecs = append(preferredCodecs, c)
		} else {
			leftCodecs = append(leftCodecs, c)
		}
	}
	if len(preferredCodecs) == 0 {
		return
	}

	reorderedCodecs := append([]webrtc.RTPCodecParameters{}, preferredCodecs...)
	if tr.Kind() == webrtc.RTPCodecTypeVideo {
		// if the client don't comply with codec order in SDP answer, only keep preferred codecs to force client to use it
		if compliesWithCodecOrderInSDPAnswer {
			reorderedCodecs = append(reorderedCodecs, leftCodecs...)
		}
	} else {
		reorderedCodecs = append(reorderedCodecs, leftCodecs...)
	}
	tr.SetCodecPreferences(reorderedCodecs)
}

func nonSimulcastRTXRepairsFromSDP(s *sdp.SessionDescription, logger logger.Logger) map[uint32]uint32 {
	rtxRepairFlows := map[uint32]uint32{}
	for _, media := range s.MediaDescriptions {
		// extract rtx repair flows from the media section for non-simulcast stream,
		// pion will handle simulcast streams by rid probe, don't need handle it here.
		var ridFound bool
		rtxPairs := make(map[uint32]uint32)
	findRTX:
		for _, attr := range media.Attributes {
			switch attr.Key {
			case "rid":
				ridFound = true
				break findRTX
			case sdp.AttrKeySSRCGroup:
				split := strings.Split(attr.Value, " ")
				if split[0] == sdp.SemanticTokenFlowIdentification {
					// Essentially lines like `a=ssrc-group:FID 2231627014 632943048` are processed by this section
					// as this declares that the second SSRC (632943048) is a rtx repair flow (RFC4588) for the first
					// (2231627014) as specified in RFC5576
					if len(split) == 3 {
						baseSsrc, err := strconv.ParseUint(split[1], 10, 32)
						if err != nil {
							logger.Warnw("Failed to parse SSRC", err, "ssrc", split[1])
							continue
						}
						rtxRepairFlow, err := strconv.ParseUint(split[2], 10, 32)
						if err != nil {
							logger.Warnw("Failed to parse SSRC", err, "ssrc", split[2])
							continue
						}
						rtxPairs[uint32(rtxRepairFlow)] = uint32(baseSsrc)
					}
				}
			}
		}
		if !ridFound {
			for rtx, base := range rtxPairs {
				rtxRepairFlows[rtx] = base
			}
		}
	}

	return rtxRepairFlows
}

// ----------------------

type iceCandidatePairStatsEncoder struct {
	stats []iceCandidatePairStats
}

func (e iceCandidatePairStatsEncoder) MarshalLogArray(arr zapcore.ArrayEncoder) error {
	for _, s := range e.stats {
		if err := arr.AppendObject(s); err != nil {
			return err
		}
	}
	return nil
}

type iceCandidatePairStats struct {
	webrtc.ICECandidatePairStats
	local, remote webrtc.ICECandidateStats
}

func (r iceCandidatePairStats) MarshalLogObject(e zapcore.ObjectEncoder) error {
	candidateToString := func(c webrtc.ICECandidateStats) string {
		return fmt.Sprintf("%s:%d %s type(%s/%s), priority(%d)", c.IP, c.Port, c.Protocol, c.CandidateType, c.RelayProtocol, c.Priority)
	}
	e.AddString("state", string(r.State))
	e.AddBool("nominated", r.Nominated)
	e.AddString("local", candidateToString(r.local))
	e.AddString("remote", candidateToString(r.remote))
	e.AddUint64("requestsSent", r.RequestsSent)
	e.AddUint64("responsesReceived", r.ResponsesReceived)
	e.AddUint64("requestsReceived", r.RequestsReceived)
	e.AddUint64("responsesSent", r.ResponsesSent)
	e.AddTime("firstRequestSentAt", r.FirstRequestTimestamp.Time())
	e.AddTime("lastRequestSentAt", r.LastRequestTimestamp.Time())
	e.AddTime("firstResponseReceivedAt", r.FirstResponseTimestamp.Time())
	e.AddTime("lastResponseReceivedAt", r.LastResponseTimestamp.Time())
	e.AddTime("firstRequestReceivedAt", r.FirstRequestReceivedTimestamp.Time())
	e.AddTime("lastRequestReceivedAt", r.LastRequestReceivedTimestamp.Time())

	return nil
}
