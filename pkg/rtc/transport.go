package rtc

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/go-logr/logr"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	serverlogger "github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const (
	LossyDataChannel    = "_lossy"
	ReliableDataChannel = "_reliable"

	negotiationFrequency       = 150 * time.Millisecond
	negotiationFailedTimout    = 15 * time.Second
	dtlsRetransmissionInterval = 100 * time.Millisecond

	iceDisconnectedTimeout = 10 * time.Second // compatible for ice-lite with firefox client
	iceFailedTimeout       = 25 * time.Second // pion's default
	iceKeepaliveInterval   = 2 * time.Second  // pion's default

	shortConnectionThreshold = 90 * time.Second
)

var (
	ErrIceRestartWithoutLocalSDP = errors.New("ICE restart without local SDP settled")
)

const (
	negotiationStateNone = iota
	// waiting for client answer
	negotiationStateClient
	// need to Negotiate again
	negotiationRetry
)

type SimulcastTrackInfo struct {
	Mid string
	Rid string
}

// PCTransport is a wrapper around PeerConnection, with some helper methods
type PCTransport struct {
	params TransportParams
	pc     *webrtc.PeerConnection
	me     *webrtc.MediaEngine

	lock sync.RWMutex

	reliableDC       *webrtc.DataChannel
	reliableDCOpened bool
	lossyDC          *webrtc.DataChannel
	lossyDCOpened    bool
	onDataPacket     func(kind livekit.DataPacket_Kind, data []byte)

	iceConnectedAt time.Time
	connectedAt    time.Time

	onFullyEstablished func()

	pendingCandidates          []webrtc.ICECandidateInit
	debouncedNegotiate         func(func())
	negotiationPending         map[livekit.ParticipantID]bool
	onICECandidate             func(c *webrtc.ICECandidate)
	onOffer                    func(offer webrtc.SessionDescription)
	onAnswer                   func(offer webrtc.SessionDescription)
	onRemoteDescriptionSettled func() error
	onInitialConnected         func()
	onFailed                   func(isShortLived bool)
	restartAfterGathering      bool
	restartAtNextOffer         bool
	negotiationState           int
	negotiateCounter           atomic.Int32
	signalStateCheckTimer      *time.Timer
	onNegotiationFailed        func()

	// stream allocator for subscriber PC
	streamAllocator *sfu.StreamAllocator

	previousAnswer *webrtc.SessionDescription

	preferTCP bool

	currentOfferIceCredential string // ice user:pwd, for publish side ice restart checking
	pendingRestartIceOffer    *webrtc.SessionDescription
}

type TransportParams struct {
	ParticipantID           livekit.ParticipantID
	ParticipantIdentity     livekit.ParticipantIdentity
	ProtocolVersion         types.ProtocolVersion
	Target                  livekit.SignalTarget
	Config                  *WebRTCConfig
	CongestionControlConfig config.CongestionControlConfig
	Telemetry               telemetry.TelemetryService
	EnabledCodecs           []*livekit.Codec
	Logger                  logger.Logger
	SimTracks               map[uint32]SimulcastTrackInfo
}

func newPeerConnection(params TransportParams, onBandwidthEstimator func(estimator cc.BandwidthEstimator)) (*webrtc.PeerConnection, *webrtc.MediaEngine, error) {
	var directionConfig DirectionConfig
	if params.Target == livekit.SignalTarget_PUBLISHER {
		directionConfig = params.Config.Publisher
	} else {
		directionConfig = params.Config.Subscriber
	}
	me, err := createMediaEngine(params.EnabledCodecs, directionConfig)
	if err != nil {
		return nil, nil, err
	}

	se := params.Config.SettingEngine
	se.DisableMediaEngineCopy(true)
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
	if !params.ProtocolVersion.SupportsICELite() {
		se.SetLite(false)
	}
	se.SetDTLSRetransmissionInterval(dtlsRetransmissionInterval)
	se.SetICETimeouts(iceDisconnectedTimeout, iceFailedTimeout, iceKeepaliveInterval)

	lf := serverlogger.NewLoggerFactory(logr.Logger(params.Logger))
	if lf != nil {
		se.LoggerFactory = lf
	}

	ir := &interceptor.Registry{}
	if params.Target == livekit.SignalTarget_SUBSCRIBER {
		isSendSideBWE := false
		for _, ext := range directionConfig.RTPHeaderExtension.Video {
			if ext == sdp.TransportCCURI {
				isSendSideBWE = true
				break
			}
		}
		for _, ext := range directionConfig.RTPHeaderExtension.Audio {
			if ext == sdp.TransportCCURI {
				isSendSideBWE = true
				break
			}
		}

		if isSendSideBWE {
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
	if len(params.SimTracks) > 0 {
		f, err := NewUnhandleSimulcastInterceptorFactory(UnhandleSimulcastTracks(params.SimTracks))
		if err != nil {
			params.Logger.Errorw("NewUnhandleSimulcastInterceptorFactory failed", err)
		} else {
			ir.Add(f)
		}
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(ir),
	)
	pc, err := api.NewPeerConnection(params.Config.Configuration)
	return pc, me, err
}

func NewPCTransport(params TransportParams) (*PCTransport, error) {
	t := &PCTransport{
		params:             params,
		debouncedNegotiate: debounce.New(negotiationFrequency),
		negotiationState:   negotiationStateNone,
		negotiationPending: make(map[livekit.ParticipantID]bool),
	}
	if params.Target == livekit.SignalTarget_SUBSCRIBER {
		t.streamAllocator = sfu.NewStreamAllocator(sfu.StreamAllocatorParams{
			Config: params.CongestionControlConfig,
			Logger: params.Logger,
		})
		t.streamAllocator.Start()
	}

	if err := t.createPeerConnection(); err != nil {
		return nil, err
	}

	return t, nil
}

func (t *PCTransport) Logger() logger.Logger {
	return t.params.Logger
}

func (t *PCTransport) setICEConnectedAt(at time.Time) {
	t.lock.Lock()
	if t.iceConnectedAt.IsZero() {
		//
		// Record initial connection time.
		// This prevents reset of connected at time if ICE goes `Connected` -> `Disconnected` -> `Connected`.
		//
		t.iceConnectedAt = at
		prometheus.ServiceOperationCounter.WithLabelValues("ice_connection", "success", "").Add(1)
	}
	t.lock.Unlock()
}

func (t *PCTransport) isShortConnection(at time.Time) (bool, time.Duration) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if t.iceConnectedAt.IsZero() {
		return false, 0
	}

	duration := at.Sub(t.iceConnectedAt)
	return duration < shortConnectionThreshold, duration
}

func (t *PCTransport) getSelectedPair() (*webrtc.ICECandidatePair, error) {
	sctp := t.pc.SCTP()
	if sctp == nil {
		return nil, errors.New("no SCTP")
	}

	dtlsTransport := sctp.Transport()
	if dtlsTransport == nil {
		return nil, errors.New("no DTLS transport")
	}

	iceTransport := dtlsTransport.ICETransport()
	if iceTransport == nil {
		return nil, errors.New("no ICE transport")
	}

	return iceTransport.GetSelectedCandidatePair()
}

func (t *PCTransport) setConnectedAt(at time.Time) bool {
	t.lock.Lock()
	if !t.connectedAt.IsZero() {
		t.lock.Unlock()
		return false
	}

	t.connectedAt = at
	prometheus.ServiceOperationCounter.WithLabelValues("peer_connection", "success", "").Add(1)
	t.lock.Unlock()
	return true
}

func (t *PCTransport) onICEGatheringStateChange(state webrtc.ICEGathererState) {
	if state != webrtc.ICEGathererStateComplete {
		return
	}

	go func() {
		t.lock.Lock()
		if t.restartAfterGathering {
			t.params.Logger.Debugw("restarting ICE after ICE gathering")
			if err := t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true}); err != nil {
				t.params.Logger.Warnw("could not restart ICE", err)
			}
			t.lock.Unlock()
		} else if t.pendingRestartIceOffer != nil {
			t.params.Logger.Debugw("accept remote restart ice offer after ICE gathering")
			offer := t.pendingRestartIceOffer
			t.pendingRestartIceOffer = nil
			t.lock.Unlock()
			if err := t.SetRemoteDescription(*offer); err != nil {
				t.params.Logger.Warnw("could not accept remote restart ice offer", err)
			}
		} else {
			t.lock.Unlock()
		}
	}()
}

func (t *PCTransport) onICECandidateTrickle(c *webrtc.ICECandidate) {
	t.lock.RLock()
	if t.preferTCP && c != nil && c.Protocol != webrtc.ICEProtocolTCP {
		t.params.Logger.Infow("filtering out local candidate", "candidate", c.String())
		t.lock.RUnlock()
		return
	}
	t.lock.RUnlock()

	if t.onICECandidate != nil {
		t.onICECandidate(c)
	}
}

func (t *PCTransport) handleConnectionFailed() {
	isShort, duration := t.isShortConnection(time.Now())
	if isShort {
		pair, err := t.getSelectedPair()
		if err != nil {
			t.params.Logger.Errorw("short ICE connection", err, "duration", duration)
		} else {
			t.params.Logger.Infow("short ICE connection", "pair", pair, "duration", duration)
		}
	}

	if t.onFailed != nil {
		t.onFailed(isShort)
	}
}

func (t *PCTransport) onICEConnectionStateChange(state webrtc.ICEConnectionState) {
	t.params.Logger.Infow("ice connection state change", "state", state.String())
	switch state {
	case webrtc.ICEConnectionStateConnected:
		t.setICEConnectedAt(time.Now())
		if pair, err := t.getSelectedPair(); err != nil {
			t.params.Logger.Errorw("error getting selected ICE candidate pair", err)
		} else {
			t.params.Logger.Infow("selected ICE candidate pair", "pair", pair)
		}
	case webrtc.ICEConnectionStateFailed:
		t.handleConnectionFailed()
	}
}

func (t *PCTransport) onPeerConnectionStateChange(state webrtc.PeerConnectionState) {
	t.params.Logger.Infow("peer connection state change", "state", state.String())
	switch state {
	case webrtc.PeerConnectionStateConnected:
		isInitialConnection := t.setConnectedAt(time.Now())
		if isInitialConnection {
			if t.onInitialConnected != nil {
				t.onInitialConnected()
			}

			t.maybeNotifyFullyEstablished()
		}
	case webrtc.PeerConnectionStateFailed:
		t.handleConnectionFailed()
	}
}

func (t *PCTransport) onDataChannel(dc *webrtc.DataChannel) {
	switch dc.Label() {
	case ReliableDataChannel:
		t.lock.Lock()
		t.reliableDC = dc
		t.reliableDCOpened = true
		t.lock.Unlock()
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if t.onDataPacket != nil {
				t.onDataPacket(livekit.DataPacket_RELIABLE, msg.Data)
			}
		})

		t.maybeNotifyFullyEstablished()
	case LossyDataChannel:
		t.lock.Lock()
		t.lossyDC = dc
		t.lossyDCOpened = true
		t.lock.Unlock()
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if t.onDataPacket != nil {
				t.onDataPacket(livekit.DataPacket_LOSSY, msg.Data)
			}
		})

		t.maybeNotifyFullyEstablished()
	default:
		t.params.Logger.Warnw("unsupported datachannel added", nil, "label", dc.Label())
	}
}

func (t *PCTransport) maybeNotifyFullyEstablished() {
	t.lock.RLock()
	fullyEstablished := t.reliableDCOpened && t.lossyDCOpened && !t.connectedAt.IsZero()
	t.lock.RUnlock()

	if fullyEstablished && t.onFullyEstablished != nil {
		t.onFullyEstablished()
	}
}

func (t *PCTransport) SetPreferTCP(preferTCP bool) {
	t.lock.Lock()
	t.preferTCP = preferTCP
	t.lock.Unlock()
}

func (t *PCTransport) createPeerConnection() error {
	var bwe cc.BandwidthEstimator
	pc, me, err := newPeerConnection(t.params, func(estimator cc.BandwidthEstimator) {
		bwe = estimator
	})
	if err != nil {
		return err
	}

	t.pc = pc
	t.pc.OnICEGatheringStateChange(t.onICEGatheringStateChange)
	t.pc.OnICEConnectionStateChange(t.onICEConnectionStateChange)
	t.pc.OnICECandidate(t.onICECandidateTrickle)

	t.pc.OnConnectionStateChange(t.onPeerConnectionStateChange)

	t.pc.OnDataChannel(t.onDataChannel)

	t.me = me

	if bwe != nil && t.streamAllocator != nil {
		t.streamAllocator.SetBandwidthEstimator(bwe)
	}

	return nil
}

func (t *PCTransport) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if t.pc.RemoteDescription() == nil {
		t.lock.Lock()
		t.pendingCandidates = append(t.pendingCandidates, candidate)
		t.lock.Unlock()
		return nil
	}

	t.lock.RLock()
	if t.preferTCP && !strings.Contains(candidate.Candidate, "tcp") {
		t.lock.RUnlock()
		t.params.Logger.Infow("filtering out remote candidate", "candidate", candidate.Candidate)
		return nil
	}
	t.lock.RUnlock()

	t.params.Logger.Infow("add candidate ", "candidate", candidate.Candidate)
	return t.pc.AddICECandidate(candidate)
}

func (t *PCTransport) PeerConnection() *webrtc.PeerConnection {
	return t.pc
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

	t.lock.Lock()
	switch dc.Label() {
	case ReliableDataChannel:
		t.reliableDC = dc
		t.reliableDC.OnOpen(func() {
			t.params.Logger.Debugw("reliable data channel open")
			t.lock.Lock()
			t.reliableDCOpened = true
			t.lock.Unlock()

			t.maybeNotifyFullyEstablished()
		})
	case LossyDataChannel:
		t.lossyDC = dc
		t.lossyDC.OnOpen(func() {
			t.params.Logger.Debugw("lossy data channel open")
			t.lock.Lock()
			t.lossyDCOpened = true
			t.lock.Unlock()

			t.maybeNotifyFullyEstablished()
		})
	default:
		t.params.Logger.Errorw("unknown data channel label", nil, "label", dc.Label())
	}
	t.lock.Unlock()

	return nil
}

func (t *PCTransport) CreateDataChannelIfEmpty(dcLabel string, dci *webrtc.DataChannelInit) (label string, id uint16, existing bool, err error) {
	t.lock.RLock()
	var dc *webrtc.DataChannel
	switch dcLabel {
	case ReliableDataChannel:
		dc = t.reliableDC
	case LossyDataChannel:
		dc = t.lossyDC
	default:
		t.params.Logger.Errorw("unknown data channel label", nil, "label", label)
		err = errors.New("unknown data channel label")
	}
	t.lock.RUnlock()
	if err != nil {
		return
	}

	if dc != nil {
		return dc.Label(), *dc.ID(), true, nil
	}

	dc, err = t.pc.CreateDataChannel(label, dci)
	if err != nil {
		return
	}

	t.onDataChannel(dc)
	return dc.Label(), *dc.ID(), false, nil
}

// IsEstablished returns true if the PeerConnection has been established
func (t *PCTransport) IsEstablished() bool {
	return t.pc.ConnectionState() != webrtc.PeerConnectionStateNew
}

func (t *PCTransport) HasEverConnected() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return !t.connectedAt.IsZero()
}

func (t *PCTransport) WriteRTCP(pkts []rtcp.Packet) error {
	return t.pc.WriteRTCP(pkts)
}

func (t *PCTransport) SendDataPacket(dp *livekit.DataPacket) error {
	data, err := proto.Marshal(dp)
	if err != nil {
		return err
	}

	var dc *webrtc.DataChannel
	t.lock.RLock()
	if dp.Kind == livekit.DataPacket_RELIABLE {
		dc = t.reliableDC
	} else {
		dc = t.lossyDC
	}
	t.lock.RUnlock()

	if dc == nil {
		return ErrDataChannelUnavailable
	}

	return dc.Send(data)
}

func (t *PCTransport) Close() {
	t.lock.Lock()
	if t.signalStateCheckTimer != nil {
		t.signalStateCheckTimer.Stop()
		t.signalStateCheckTimer = nil
	}
	t.lock.Unlock()

	if t.streamAllocator != nil {
		t.streamAllocator.Stop()
	}

	_ = t.pc.Close()
}

func (t *PCTransport) SetRemoteDescription(sd webrtc.SessionDescription) error {
	t.lock.Lock()

	var (
		iceCredential   string
		offerRestartICE bool
	)
	if sd.Type == webrtc.SDPTypeOffer {
		var err error
		iceCredential, offerRestartICE, err = t.isRemoteOfferRestartICE(sd)
		if err != nil {
			t.Logger().Errorw("check remote offer restart ice failed", err)
			t.lock.Unlock()
			return err
		}
	}

	if offerRestartICE && t.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
		t.Logger().Debugw("remote offer restart ice while ice gathering")
		t.pendingRestartIceOffer = &sd
		t.lock.Unlock()
		return nil
	}

	// filter before setting remote description so that pion does not see filtered remote candidates
	if t.preferTCP {
		t.params.Logger.Infow("remote description (unfiltered)", "type", sd.Type, "sdp", sd.SDP)
	}
	sd = t.filterCandidates(sd)
	if t.preferTCP {
		t.params.Logger.Infow("remote description (filtered)", "type", sd.Type, "sdp", sd.SDP)
	}
	if err := t.pc.SetRemoteDescription(sd); err != nil {
		t.lock.Unlock()
		return err
	}

	if t.currentOfferIceCredential == "" || offerRestartICE {
		t.currentOfferIceCredential = iceCredential
	}

	// negotiated, reset flag
	lastState := t.negotiationState
	t.negotiationState = negotiationStateNone

	if t.signalStateCheckTimer != nil {
		t.signalStateCheckTimer.Stop()
		t.signalStateCheckTimer = nil
	}

	for _, c := range t.pendingCandidates {
		if err := t.pc.AddICECandidate(c); err != nil {
			t.lock.Unlock()
			return err
		}
	}
	t.pendingCandidates = nil

	// only initiate when we are the offerer
	if lastState == negotiationRetry && sd.Type == webrtc.SDPTypeAnswer {
		t.params.Logger.Debugw("re-negotiate after receiving answer")
		if err := t.createAndSendOffer(nil); err != nil {
			t.params.Logger.Errorw("could not negotiate", err)
		}
	}
	onRemoteDescriptionSettled := t.onRemoteDescriptionSettled
	t.lock.Unlock()

	if onRemoteDescriptionSettled != nil {
		return onRemoteDescriptionSettled()
	}
	return nil
}

func (t *PCTransport) isRemoteOfferRestartICE(sd webrtc.SessionDescription) (string, bool, error) {
	parsed, err := sd.Unmarshal()
	if err != nil {
		return "", false, err
	}
	user, pwd, err := extractICECredential(parsed)
	if err != nil {
		return "", false, err
	}

	credential := fmt.Sprintf("%s:%s", user, pwd)
	// ice credential changed, remote offer restart ice
	restartICE := t.currentOfferIceCredential != "" && t.currentOfferIceCredential != credential
	return credential, restartICE, nil
}

func (t *PCTransport) OnICECandidate(f func(c *webrtc.ICECandidate)) {
	t.onICECandidate = f
}

func (t *PCTransport) OnInitialConnected(f func()) {
	t.onInitialConnected = f
}

func (t *PCTransport) OnFullyEstablished(f func()) {
	t.onFullyEstablished = f
}

func (t *PCTransport) OnFailed(f func(isShortLived bool)) {
	t.onFailed = f
}

func (t *PCTransport) OnTrack(f func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver)) {
	t.pc.OnTrack(f)
}

func (t *PCTransport) OnDataPacket(f func(kind livekit.DataPacket_Kind, data []byte)) {
	t.onDataPacket = f
}

// OnOffer is called when the PeerConnection starts negotiation and prepares an offer
func (t *PCTransport) OnOffer(f func(sd webrtc.SessionDescription)) {
	t.onOffer = f
}

func (t *PCTransport) OnAnswer(f func(sd webrtc.SessionDescription)) {
	t.onAnswer = f
}

func (t *PCTransport) OnRemoteDescripitonSettled(f func() error) {
	t.lock.Lock()
	t.onRemoteDescriptionSettled = f
	t.lock.Unlock()
}

func (t *PCTransport) OnNegotiationFailed(f func()) {
	t.onNegotiationFailed = f
}

func (t *PCTransport) AddNegotiationPending(publisherID livekit.ParticipantID) {
	t.lock.Lock()
	t.negotiationPending[publisherID] = true
	t.lock.Unlock()
}

func (t *PCTransport) Negotiate(force bool) {
	if force {
		t.debouncedNegotiate(func() {
			// no op to cancel pending negotiation
		})
		if err := t.CreateAndSendOffer(nil); err != nil {
			t.params.Logger.Errorw("could not negotiate", err)
		}
	} else {
		t.debouncedNegotiate(func() {
			if err := t.CreateAndSendOffer(nil); err != nil {
				t.params.Logger.Errorw("could not negotiate", err)
			}
		})
	}
}

func (t *PCTransport) IsNegotiationPending(publisherID livekit.ParticipantID) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.negotiationPending[publisherID]
}

func (t *PCTransport) configureReceiverDTX(enableDTX bool) {
	//
	// DTX (Discontinuous Transmission) allows audio bandwidth saving
	// by not sending packets during silence periods.
	//
	// Publisher side DTX can enabled by including `usedtx=1` in
	// the `fmtp` line corresponding to audio codec (Opus) in SDP.
	// By doing this in the SDP `answer`, it can be controlled from
	// server side and avoid doing it in all the client SDKs.
	//
	// Ideally, a publisher should be able to specify per audio
	// track if DTX should be enabled. But, translating the
	// DTX preference of publisher to the correct transceiver
	// is non-deterministic due to the lack of a synchronizing id
	// like the track id.
	//
	// The codec preference to set DTX needs to be done
	//   - after calling `SetRemoteDescription` which sets up
	//     the transceivers, but only if there are no tracks in the
	//     transceiver yet
	//   - before calling `CreateAnswer`
	// Due to the absence of tracks when it is required to set DTX,
	// it is not possible to cross reference against a pending track
	// with the same track id.
	//
	// Due to the restriction above and given that in practice
	// most of the time there is going to be only one audio track
	// that is published, do the following
	//    - if there is no pending audio track, no-op
	//    - if there are no audio transceivers without tracks, no-op
	//    - else, apply the DTX setting from pending audio track
	//      to the audio transceiver without any track
	//
	// NOTE: The above logic will fail if there is an `offer` SDP with
	// multiple audio tracks. At that point, there might be a need to
	// rely on something like order of tracks. TODO
	//
	transceivers := t.pc.GetTransceivers()
	for _, transceiver := range transceivers {
		if transceiver.Kind() != webrtc.RTPCodecTypeAudio {
			continue
		}

		receiver := transceiver.Receiver()
		if receiver == nil || receiver.Track() != nil {
			continue
		}

		var modifiedReceiverCodecs []webrtc.RTPCodecParameters

		receiverCodecs := receiver.GetParameters().Codecs
		for _, receiverCodec := range receiverCodecs {
			if receiverCodec.MimeType == webrtc.MimeTypeOpus {
				fmtpUseDTX := "usedtx=1"
				// remove occurrence in the middle
				sdpFmtpLine := strings.ReplaceAll(receiverCodec.SDPFmtpLine, fmtpUseDTX+";", "")
				// remove occurrence at the end
				sdpFmtpLine = strings.ReplaceAll(sdpFmtpLine, fmtpUseDTX, "")
				if enableDTX {
					sdpFmtpLine += ";" + fmtpUseDTX
				}
				receiverCodec.SDPFmtpLine = sdpFmtpLine
			}
			modifiedReceiverCodecs = append(modifiedReceiverCodecs, receiverCodec)
		}

		//
		// As `SetCodecPreferences` on a transceiver replaces all codecs,
		// cycle through sender codecs also and add them before calling
		// `SetCodecPreferences`
		//
		var senderCodecs []webrtc.RTPCodecParameters
		sender := transceiver.Sender()
		if sender != nil {
			senderCodecs = sender.GetParameters().Codecs
		}

		err := transceiver.SetCodecPreferences(append(modifiedReceiverCodecs, senderCodecs...))
		if err != nil {
			t.params.Logger.Warnw("failed to SetCodecPreferences", err)
		}
	}
}

func (t *PCTransport) CreateAndSendAnswer(enableDTX bool) error {
	t.lock.RLock()
	defer t.lock.RUnlock()

	t.configureReceiverDTX(enableDTX)

	answer, err := t.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}

	if t.preferTCP {
		t.params.Logger.Infow("local answer (unfiltered)", "sdp", answer.SDP)
	}
	if err = t.pc.SetLocalDescription(answer); err != nil {
		return err
	}

	//
	// Filter after setting local description as pion expects the answer
	// to match between CreateAnswer and SetLocalDescription.
	// Filtered answer is sent to remote so that remote does not
	// see filtered candidates.
	//
	answer = t.filterCandidates(answer)
	if t.preferTCP {
		t.params.Logger.Infow("local answer (filtered)", "sdp", answer.SDP)
	}

	if t.onAnswer != nil {
		go t.onAnswer(answer)
	}

	return nil
}

func (t *PCTransport) CreateAndSendOffer(options *webrtc.OfferOptions) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.createAndSendOffer(options)
}

// creates and sends offer assuming lock has been acquired
func (t *PCTransport) createAndSendOffer(options *webrtc.OfferOptions) error {
	if t.onOffer == nil {
		return nil
	}
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return nil
	}

	iceRestart := (options != nil && options.ICERestart) || t.restartAtNextOffer

	// if restart is requested, and we are not ready, then continue afterwards
	if iceRestart {
		if t.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			t.params.Logger.Debugw("restart ICE after gathering")
			t.restartAfterGathering = true
			return nil
		}
		t.params.Logger.Debugw("restarting ICE")
	}

	if iceRestart && t.negotiationState != negotiationStateNone {
		currentSD := t.pc.CurrentRemoteDescription()
		if currentSD == nil {
			// restart without current remote description, send current local description again to try recover
			offer := t.pc.LocalDescription()
			if offer == nil {
				// it should not happen, log just in case
				t.params.Logger.Warnw("ice restart without local offer", nil)
				return ErrIceRestartWithoutLocalSDP
			} else {
				t.negotiationState = negotiationRetry
				t.restartAtNextOffer = true
				go t.onOffer(*offer)
				return nil
			}
		} else {
			// recover by re-applying the last answer
			t.params.Logger.Infow("recovering from client negotiation state on ICE restart")
			if err := t.pc.SetRemoteDescription(*currentSD); err != nil {
				prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "remote_description").Add(1)
				return err
			}
		}
	} else {
		// when there's an ongoing negotiation, let it finish and not disrupt its state
		if t.negotiationState == negotiationStateClient {
			t.params.Logger.Infow("skipping negotiation, trying again later")
			t.negotiationState = negotiationRetry
			return nil
		} else if t.negotiationState == negotiationRetry {
			// already set to retry, we can safely skip this attempt
			return nil
		}
	}

	ensureICERestart := func(options *webrtc.OfferOptions) *webrtc.OfferOptions {
		if options == nil {
			options = &webrtc.OfferOptions{}
		}
		options.ICERestart = true
		return options
	}

	if t.previousAnswer != nil {
		t.previousAnswer = nil
		options = ensureICERestart(options)
	}

	if t.restartAtNextOffer {
		t.restartAtNextOffer = false
		options = ensureICERestart(options)
	}

	offer, err := t.pc.CreateOffer(options)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "create").Add(1)
		t.params.Logger.Errorw("could not create offer", err)
		return err
	}

	if t.preferTCP {
		t.params.Logger.Infow("local offer (unfiltered)", "sdp", offer.SDP)
	}
	err = t.pc.SetLocalDescription(offer)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "local_description").Add(1)
		t.params.Logger.Errorw("could not set local description", err)
		return err
	}

	//
	// Filter after setting local description as pion expects the offer
	// to match between CreateOffer and SetLocalDescription.
	// Filtered offer is sent to remote so that remote does not
	// see filtered candidates.
	//
	offer = t.filterCandidates(offer)
	if t.preferTCP {
		t.params.Logger.Infow("local offer (filtered)", "sdp", offer.SDP)
	}

	// indicate waiting for client
	t.negotiationState = negotiationStateClient
	t.restartAfterGathering = false
	t.negotiationPending = make(map[livekit.ParticipantID]bool)

	negotiateVersion := t.negotiateCounter.Inc()
	if t.signalStateCheckTimer != nil {
		t.signalStateCheckTimer.Stop()
		t.signalStateCheckTimer = nil
	}
	t.signalStateCheckTimer = time.AfterFunc(negotiationFailedTimout, func() {
		t.lock.RLock()
		failed := t.negotiationState != negotiationStateNone
		t.lock.RUnlock()
		if t.negotiateCounter.Load() == negotiateVersion && failed {
			if t.onNegotiationFailed != nil {
				t.onNegotiationFailed()
			}
		}
	})

	go t.onOffer(offer)
	return nil
}

func (t *PCTransport) preparePC(previousAnswer webrtc.SessionDescription) error {
	// sticky data channel to first m-lines, if someday we don't send sdp without media streams to
	// client's subscribe pc after joining, should change this step
	parsed, err := previousAnswer.Unmarshal()
	if err != nil {
		return err
	}
	fp, fpHahs, err := extractFingerprint(parsed)
	if err != nil {
		return err
	}

	offer, err := t.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	t.pc.SetLocalDescription(offer)

	//
	// Simulate client side peer connection and set DTLS role from previous answer.
	// Role needs to be set properly (one side needs to be server and the other side
	// needs to be the client) for DTLS connection to form properly. As this is
	// trying to replicate previous setup, read from previous answer and use that role.
	//
	se := webrtc.SettingEngine{}
	se.SetAnsweringDTLSRole(extractDTLSRole(parsed))
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

	pc2.SetRemoteDescription(offer)
	ans, err := pc2.CreateAnswer(nil)
	if err != nil {
		return err
	}

	// replace client's fingerprint into dump pc's answer, for pion's dtls process, it will
	// keep the fingerprint at first call of SetRemoteDescription, if dumb pc and client pc use
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

func (t *PCTransport) initPCWithPreviousAnswer(previousAnswer webrtc.SessionDescription) error {
	parsed, err := previousAnswer.Unmarshal()
	if err != nil {
		return err
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
			// so use a dumb pc to negotiate sdp to fixed the datachannel's mid at same position with previous answer
			if err := t.preparePC(previousAnswer); err != nil {
				t.params.Logger.Errorw("prepare pc for migration failed", err)
				return err
			}
			continue
		default:
			continue
		}
		tr, err := t.pc.AddTransceiverFromKind(codecType, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		if err != nil {
			return err
		}
		tr.Stop()
		mid := getMidValue(m)
		if mid == "" {
			return errors.New("mid value not found")
		}
		tr.SetMid(mid)
	}
	return nil
}

func (t *PCTransport) OnStreamStateChange(f func(update *sfu.StreamStateUpdate) error) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.OnStreamStateChange(f)
}

func (t *PCTransport) AddTrack(subTrack types.SubscribedTrack) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.AddTrack(subTrack.DownTrack(), sfu.AddTrackParams{
		Source:      subTrack.MediaTrack().Source(),
		IsSimulcast: subTrack.MediaTrack().IsSimulcast(),
		PublisherID: subTrack.MediaTrack().PublisherID(),
	})
}

func (t *PCTransport) RemoveTrack(subTrack types.SubscribedTrack) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.RemoveTrack(subTrack.DownTrack())
}

func (t *PCTransport) SetPreviousAnswer(answer *webrtc.SessionDescription) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.pc.RemoteDescription() == nil && t.previousAnswer == nil {
		t.previousAnswer = answer
		if err := t.initPCWithPreviousAnswer(*t.previousAnswer); err != nil {
			t.params.Logger.Errorw("initPCWithPreviousAnswer failed", err)
		}
	}
}

func (t *PCTransport) filterCandidates(sd webrtc.SessionDescription) webrtc.SessionDescription {
	parsed, err := sd.Unmarshal()
	if err != nil {
		t.params.Logger.Errorw("could not unmarshal SDP to filter candidates", err)
		return sd
	}

	filterAttributes := func(attrs []sdp.Attribute) []sdp.Attribute {
		filteredAttrs := make([]sdp.Attribute, 0, len(attrs))
		for _, a := range attrs {
			if a.Key == sdp.AttrKeyCandidate {
				if t.preferTCP {
					if strings.Contains(a.Value, "tcp") {
						filteredAttrs = append(filteredAttrs, a)
					}
				} else {
					filteredAttrs = append(filteredAttrs, a)
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
		t.params.Logger.Errorw("could not marshal SDP to filter candidates", err)
		return sd
	}
	sd.SDP = string(bytes)
	return sd
}

// ---------------------------------------------

func getMidValue(media *sdp.MediaDescription) string {
	for _, attr := range media.Attributes {
		if attr.Key == sdp.AttrKeyMID {
			return attr.Value
		}
	}
	return ""
}

func extractFingerprint(desc *sdp.SessionDescription) (string, string, error) {
	fingerprints := make([]string, 0)

	if fingerprint, haveFingerprint := desc.Attribute("fingerprint"); haveFingerprint {
		fingerprints = append(fingerprints, fingerprint)
	}

	for _, m := range desc.MediaDescriptions {
		if fingerprint, haveFingerprint := m.Attribute("fingerprint"); haveFingerprint {
			fingerprints = append(fingerprints, fingerprint)
		}
	}

	if len(fingerprints) < 1 {
		return "", "", webrtc.ErrSessionDescriptionNoFingerprint
	}

	for _, m := range fingerprints {
		if m != fingerprints[0] {
			return "", "", webrtc.ErrSessionDescriptionConflictingFingerprints
		}
	}

	parts := strings.Split(fingerprints[0], " ")
	if len(parts) != 2 {
		return "", "", webrtc.ErrSessionDescriptionInvalidFingerprint
	}
	return parts[1], parts[0], nil
}

func extractDTLSRole(desc *sdp.SessionDescription) webrtc.DTLSRole {
	for _, md := range desc.MediaDescriptions {
		setup, ok := md.Attribute(sdp.AttrKeyConnectionSetup)
		if !ok {
			continue
		}

		if setup == sdp.ConnectionRoleActive.String() {
			return webrtc.DTLSRoleClient
		}

		if setup == sdp.ConnectionRolePassive.String() {
			return webrtc.DTLSRoleServer
		}
	}

	//
	// If 'setup' attribute is not available, use client role
	// as that is the default behaviour of answerers
	//
	// There seems to be some differences in how role is decided.
	// libwebrtc (Chrome) code - (https://source.chromium.org/chromium/chromium/src/+/main:third_party/webrtc/pc/jsep_transport.cc;l=592;drc=369fb686729e7eb20d2bd09717cec14269a399d7)
	// does not mention anything about ICE role when determining
	// DTLS Role.
	//
	// But, ORTC has this - https://github.com/w3c/ortc/issues/167#issuecomment-69409953
	// and pion/webrtc follows that (https://github.com/pion/webrtc/blob/e071a4eded1efd5d9b401bcfc4efacb3a2a5a53c/dtlstransport.go#L269)
	//
	// So if remote is ice-lite, pion will use DTLSRoleServer when answering
	// while browsers pick DTLSRoleClient.
	//
	return webrtc.DTLSRoleClient
}

func extractICECredential(desc *sdp.SessionDescription) (string, string, error) {
	remotePwds := []string{}
	remoteUfrags := []string{}

	if ufrag, haveUfrag := desc.Attribute("ice-ufrag"); haveUfrag {
		remoteUfrags = append(remoteUfrags, ufrag)
	}
	if pwd, havePwd := desc.Attribute("ice-pwd"); havePwd {
		remotePwds = append(remotePwds, pwd)
	}

	for _, m := range desc.MediaDescriptions {
		if ufrag, haveUfrag := m.Attribute("ice-ufrag"); haveUfrag {
			remoteUfrags = append(remoteUfrags, ufrag)
		}
		if pwd, havePwd := m.Attribute("ice-pwd"); havePwd {
			remotePwds = append(remotePwds, pwd)
		}
	}

	if len(remoteUfrags) == 0 {
		return "", "", webrtc.ErrSessionDescriptionMissingIceUfrag
	} else if len(remotePwds) == 0 {
		return "", "", webrtc.ErrSessionDescriptionMissingIcePwd
	}

	for _, m := range remoteUfrags {
		if m != remoteUfrags[0] {
			return "", "", webrtc.ErrSessionDescriptionConflictingIceUfrag
		}
	}

	for _, m := range remotePwds {
		if m != remotePwds[0] {
			return "", "", webrtc.ErrSessionDescriptionConflictingIcePwd
		}
	}

	return remoteUfrags[0], remotePwds[0], nil
}
