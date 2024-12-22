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

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/transport"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/bwe/remotebwe"
	"github.com/livekit/livekit-server/pkg/sfu/bwe/sendsidebwe"
	"github.com/livekit/livekit-server/pkg/sfu/datachannel"
	sfuinterceptor "github.com/livekit/livekit-server/pkg/sfu/interceptor"
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
	ErrNoICETransport                   = errors.New("no ICE transport")
	ErrIceRestartWithoutLocalSDP        = errors.New("ICE restart without local SDP settled")
	ErrIceRestartOnClosedPeerConnection = errors.New("ICE restart on closed peer connection")
	ErrNoTransceiver                    = errors.New("no transceiver")
	ErrNoSender                         = errors.New("no sender")
	ErrMidNotFound                      = errors.New("mid not found")
	ErrNotSynchronousPeerConnectionMode = errors.New("not using synchronous peer connection mode")
	ErrNoRemoteDescription              = errors.New("no remote description")
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

	previousAnswer *webrtc.SessionDescription
	// track id -> description map in previous offer sdp
	previousTrackDescription map[string]*trackDescription
	canReuseTransceiver      bool

	preferTCP atomic.Bool
	isClosed  atomic.Bool

	eventsQueue *utils.TypedOpsQueue[event]

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

	connectionDetails *types.ICEConnectionDetails
	selectedPair      atomic.Pointer[webrtc.ICECandidatePair]
}

type TransportParams struct {
	Handler                      transport.Handler
	ParticipantID                livekit.ParticipantID
	ParticipantIdentity          livekit.ParticipantIdentity
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
	if !params.ProtocolVersion.SupportsICELite() || !params.ClientInfo.SupportPrflxOverRelay() {
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
	if !params.ClientInfo.SupportPrflxOverRelay() && len(params.Config.NAT1To1IPs) > 0 {
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
	} else {
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
		if !strings.HasPrefix(info.MimeType, "video") {
			return
		}
		// rtx stream don't have rtcp feedback, always set twcc for rtx stream
		twccFb := strings.HasSuffix(info.MimeType, "rtx")
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

	bwe, err := t.createPeerConnection()
	if err != nil {
		return nil, err
	}

	if params.IsSendSide {
		if params.CongestionControlConfig.UseSendSideBWE {
			params.Logger.Infow("using send side BWE")
			t.bwe = sendsidebwe.NewSendSideBWE(sendsidebwe.SendSideBWEParams{
				Config: params.CongestionControlConfig.SendSideBWE,
				Logger: params.Logger,
			})
			t.pacer = pacer.NewNoQueue(params.Logger, t.bwe)
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
	prometheus.ServiceOperationCounter.WithLabelValues("peer_connection", "success", "").Add(1)
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
		switch dc.Label() {
		case ReliableDataChannel:
			kind = livekit.DataPacket_RELIABLE

		case LossyDataChannel:
			kind = livekit.DataPacket_LOSSY

		default:
			t.params.Logger.Warnw("unsupported datachannel added", nil, "label", dc.Label())
			return
		}

		rawDC, err := dc.DetachWithDeadline()
		if err != nil {
			t.params.Logger.Errorw("failed to detach data channel", err, "label", dc.Label())
			return
		}

		switch kind {
		case livekit.DataPacket_RELIABLE:
			t.lock.Lock()
			if t.reliableDC != nil {
				t.reliableDC.Close()
			}
			t.reliableDC = datachannel.NewDataChannelWriter(dc, rawDC, t.params.DatachannelSlowThreshold)
			t.reliableDCOpened = true
			t.lock.Unlock()

		case livekit.DataPacket_LOSSY:
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

				t.params.Handler.OnDataPacket(kind, buffer[:n])
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

	dataChannelReady := t.firstOfferNoDataChannel || (t.reliableDCOpened && t.lossyDCOpened)

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

func (t *PCTransport) AddTrack(trackLocal webrtc.TrackLocal, params types.AddTrackParams) (sender *webrtc.RTPSender, transceiver *webrtc.RTPTransceiver, err error) {
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
		return t.AddTransceiverFromTrack(trackLocal, params)
	}

	sender, err = t.pc.AddTrack(trackLocal)
	if err != nil {
		return
	}

	// as there is no way to get transceiver from sender, search
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

	if trackLocal.Kind() == webrtc.RTPCodecTypeAudio {
		configureAudioTransceiver(transceiver, params.Stereo, !params.Red || !t.params.ClientInfo.SupportsAudioRED())
	}
	return
}

func (t *PCTransport) AddTransceiverFromTrack(trackLocal webrtc.TrackLocal, params types.AddTrackParams) (sender *webrtc.RTPSender, transceiver *webrtc.RTPTransceiver, err error) {
	transceiver, err = t.pc.AddTransceiverFromTrack(trackLocal)
	if err != nil {
		return
	}

	sender = transceiver.Sender()
	if sender == nil {
		err = ErrNoSender
		return
	}

	if trackLocal.Kind() == webrtc.RTPCodecTypeAudio {
		configureAudioTransceiver(transceiver, params.Stereo, !params.Red || !t.params.ClientInfo.SupportsAudioRED())
	}

	return
}

func (t *PCTransport) RemoveTrack(sender *webrtc.RTPSender) error {
	return t.pc.RemoveTrack(sender)
}

func (t *PCTransport) GetMid(rtpReceiver *webrtc.RTPReceiver) string {
	for _, tr := range t.pc.GetTransceivers() {
		if tr.Receiver() == rtpReceiver {
			return tr.Mid()
		}
	}

	return ""
}

func (t *PCTransport) GetRTPReceiver(mid string) *webrtc.RTPReceiver {
	for _, tr := range t.pc.GetTransceivers() {
		if tr.Mid() == mid {
			return tr.Receiver()
		}
	}

	return nil
}

func (t *PCTransport) CreateDataChannel(label string, dci *webrtc.DataChannelInit) error {
	dc, err := t.pc.CreateDataChannel(label, dci)
	if err != nil {
		return err
	}
	var (
		dcPtr   **datachannel.DataChannelWriter[*webrtc.DataChannel]
		dcReady *bool
	)
	switch dc.Label() {
	default:
		// TODO: Appears that it's never called, so not sure what needs to be done here. We just keep the DC open?
		//       Maybe just add "reliable" parameter instead of checking the label.
		t.params.Logger.Warnw("unknown data channel label", nil, "label", dc.Label())
		return nil
	case ReliableDataChannel:
		dcPtr = &t.reliableDC
		dcReady = &t.reliableDCOpened
	case LossyDataChannel:
		dcPtr = &t.lossyDC
		dcReady = &t.lossyDCOpened
	}

	dc.OnOpen(func() {
		rawDC, err := dc.DetachWithDeadline()
		if err != nil {
			t.params.Logger.Warnw("failed to detach data channel", err)
			return
		}

		var slowThreshold int
		if dc.Label() == ReliableDataChannel {
			slowThreshold = t.params.DatachannelSlowThreshold
		}

		t.lock.Lock()
		if *dcPtr != nil {
			(*dcPtr).Close()
		}
		*dcPtr = datachannel.NewDataChannelWriter(dc, rawDC, slowThreshold)
		*dcReady = true
		t.lock.Unlock()
		t.params.Logger.Debugw(dc.Label() + " data channel open")

		t.maybeNotifyFullyEstablished()
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

// IsEstablished returns true if the PeerConnection has been established
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

func (t *PCTransport) WriteRTCP(pkts []rtcp.Packet) error {
	return t.pc.WriteRTCP(pkts)
}

func (t *PCTransport) SendDataPacket(kind livekit.DataPacket_Kind, encoded []byte) error {
	var dc *datachannel.DataChannelWriter[*webrtc.DataChannel]
	t.lock.RLock()
	if kind == livekit.DataPacket_RELIABLE {
		dc = t.reliableDC
	} else {
		dc = t.lossyDC
	}
	t.lock.RUnlock()

	if dc == nil {
		return ErrDataChannelUnavailable
	}

	if t.pc.ConnectionState() == webrtc.PeerConnectionStateFailed {
		return ErrTransportFailure
	}

	if t.params.DatachannelSlowThreshold == 0 && t.params.DataChannelMaxBufferedAmount > 0 && dc.BufferedAmountGetter().BufferedAmount() > t.params.DataChannelMaxBufferedAmount {
		return ErrDataChannelBufferFull
	}
	_, err := dc.Write(encoded)

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

	_ = t.pc.Close()

	t.clearConnTimer()

	t.lock.Lock()
	if t.reliableDC != nil {
		t.reliableDC.Close()
		t.reliableDC = nil
	}

	if t.lossyDC != nil {
		t.lossyDC.Close()
		t.lossyDC = nil
	}
	t.lock.Unlock()
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

func (t *PCTransport) HandleRemoteDescription(sd webrtc.SessionDescription) error {
	if t.params.UseOneShotSignallingMode {
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
		}
		return err
	}

	t.postEvent(event{
		signal: signalRemoteDescriptionReceived,
		data:   &sd,
	})
	return nil
}

func (t *PCTransport) GetAnswer() (webrtc.SessionDescription, error) {
	if !t.params.UseOneShotSignallingMode {
		return webrtc.SessionDescription{}, ErrNotSynchronousPeerConnectionMode
	}

	prd := t.pc.PendingRemoteDescription()
	if prd == nil || prd.Type != webrtc.SDPTypeOffer {
		return webrtc.SessionDescription{}, ErrNoRemoteDescription
	}

	answer, err := t.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}

	if err = t.pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, err
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

	return *cld, nil
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
		t.updateLastNeogitateLocked()

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
				t.updateLastNeogitateLocked()
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

func (t *PCTransport) updateLastNeogitateLocked() {
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

	t.streamAllocator.AddTrack(subTrack.DownTrack(), streamallocator.AddTrackParams{
		Source:      subTrack.MediaTrack().Source(),
		IsSimulcast: subTrack.MediaTrack().IsSimulcast(),
		PublisherID: subTrack.MediaTrack().PublisherID(),
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

	// replace client's fingerprint into dump pc's answer, for pion's dtls process, it will
	// keep the fingerprint at first call of SetRemoteDescription, if dummy pc and client pc use
	// different fingerprint, that will cause pion denied dtls data after handshake with client
	// complete (can't pass fingerprint change).
	// in this step, we don't established connection with dump pc(no candidate swap), just use
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
			// for pion generate unmatched sdp, it always appends data channel to last m-lines,
			// that not consistent with our previous answer that data channel might at middle-line
			// because sdp can negotiate multi times before migration.(it will sticky to the last m-line atfirst negotiate)
			// so use a dummy pc to negotiate sdp to fixed the datachannel's mid at same position with previous answer
			if err := t.preparePC(previousAnswer); err != nil {
				t.params.Logger.Warnw("prepare pc for migration failed", err)
				return senders, err
			}
			continue
		default:
			continue
		}
		tr, err := t.pc.AddTransceiverFromKind(codecType, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
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
		tr.SetSender(tr.Sender(), nil)
	}
	return senders, nil
}

func (t *PCTransport) SetPreviousSdp(offer, answer *webrtc.SessionDescription) {
	// when there is no previous answer, cannot migrate, force a full reconnect
	if answer == nil {
		t.params.Handler.OnNegotiationFailed()
		return
	}

	t.lock.Lock()
	if t.pc.RemoteDescription() == nil && t.previousAnswer == nil {
		t.previousAnswer = answer
		if senders, err := t.initPCWithPreviousAnswer(*t.previousAnswer); err != nil {
			t.params.Logger.Warnw("initPCWithPreviousAnswer failed", err)
			t.lock.Unlock()

			t.params.Handler.OnNegotiationFailed()
			return
		} else if offer != nil {
			// in migration case, can't reuse transceiver before negotiated except track subscribed at previous node
			t.canReuseTransceiver = false
			if err := t.parseTrackMid(*offer, senders); err != nil {
				t.params.Logger.Warnw("parse previous offer failed", err, "offer", offer.SDP)
			}
		}
	}
	// disable fast negotiation temporarily after migration to avoid sending offer
	// contains part of subscribed tracks before migration, let the subscribed track
	// resume at the same time.
	t.lastNegotiate = time.Now().Add(iceFailedTimeoutTotal)
	t.lock.Unlock()
}

func (t *PCTransport) parseTrackMid(offer webrtc.SessionDescription, senders map[string]*webrtc.RTPSender) error {
	parsed, err := offer.Unmarshal()
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
			t.previousTrackDescription[trackID] = &trackDescription{
				mid:    mid,
				sender: senders[mid],
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
				e.params.Logger.Warnw("error handling event", err, "event", e.String())
				e.params.Handler.OnNegotiationFailed()
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
		t.params.Logger.Warnw("failed to add cached ICE candidate", err, "candidate", c)
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
			t.params.Logger.Infow(
				"negotiation timed out",
				"localCurrent", t.pc.CurrentLocalDescription(),
				"localPending", t.pc.PendingLocalDescription(),
				"remoteCurrent", t.pc.CurrentRemoteDescription(),
				"remotePending", t.pc.PendingRemoteDescription(),
			)
			t.params.Handler.OnNegotiationFailed()
		}
	})
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

		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "create").Add(1)
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

		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "local_description").Add(1)
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

	if err := t.params.Handler.OnOffer(offer); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "write_message").Add(1)
		return errors.Wrap(err, "could not send offer")
	}

	prometheus.ServiceOperationCounter.WithLabelValues("offer", "success", "").Add(1)
	return t.localDescriptionSent()
}

func (t *PCTransport) handleSendOffer(_ event) error {
	return t.createAndSendOffer(nil)
}

func (t *PCTransport) handleRemoteDescriptionReceived(e event) error {
	sd := e.data.(*webrtc.SessionDescription)
	if sd.Type == webrtc.SDPTypeOffer {
		return t.handleRemoteOfferReceived(sd)
	} else {
		return t.handleRemoteAnswerReceived(sd)
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
		prometheus.ServiceOperationCounter.WithLabelValues(sdpType, "error", "remote_description").Add(1)
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
	answer, err := t.pc.CreateAnswer(nil)
	if err != nil {
		if errors.Is(err, webrtc.ErrConnectionClosed) {
			t.params.Logger.Warnw("trying to create answer on closed peer connection", nil)
			return nil
		}

		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "create").Add(1)
		return errors.Wrap(err, "create answer failed")
	}

	preferTCP := t.preferTCP.Load()
	if preferTCP {
		t.params.Logger.Debugw("local answer (unfiltered)", "sdp", answer.SDP)
	}

	if err = t.pc.SetLocalDescription(answer); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "local_description").Add(1)
		return errors.Wrap(err, "setting local description failed")
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

	if err := t.params.Handler.OnAnswer(answer); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "write_message").Add(1)
		return errors.Wrap(err, "could not send answer")
	}

	prometheus.ServiceOperationCounter.WithLabelValues("answer", "success", "").Add(1)
	return t.localDescriptionSent()
}

func (t *PCTransport) handleRemoteOfferReceived(sd *webrtc.SessionDescription) error {
	parsed, err := sd.Unmarshal()
	if err != nil {
		return nil
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

	if err := t.setRemoteDescription(*sd); err != nil {
		return err
	}
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

func (t *PCTransport) handleRemoteAnswerReceived(sd *webrtc.SessionDescription) error {
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
			err := t.params.Handler.OnOffer(*offer)
			if err != nil {
				prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "write_message").Add(1)
			} else {
				prometheus.ServiceOperationCounter.WithLabelValues("offer", "success", "").Add(1)
			}
			return err
		}
	} else {
		// recover by re-applying the last answer
		t.params.Logger.Infow("recovering from client negotiation state on ICE restart")
		if err := t.pc.SetRemoteDescription(*currentRemoteDescription); err != nil {
			prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "remote_description").Add(1)
			return errors.Wrap(err, "set remote description failed")
		} else {
			t.setNegotiationState(transport.NegotiationStateNone)
			return t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true})
		}
	}
}

func (t *PCTransport) handleICERestart(_ event) error {
	return t.doICERestart()
}

// configure subscriber transceiver for audio stereo and nack
// pion doesn't support per transciver codec configuration, so the nack of this session will be disabled
// forever once it is first disabled by a transceiver.
func configureAudioTransceiver(tr *webrtc.RTPTransceiver, stereo bool, nack bool) {
	sender := tr.Sender()
	if sender == nil {
		return
	}
	// enable stereo
	codecs := sender.GetParameters().Codecs
	configCodecs := make([]webrtc.RTPCodecParameters, 0, len(codecs))
	for _, c := range codecs {
		if strings.EqualFold(c.MimeType, webrtc.MimeTypeOpus) {
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
