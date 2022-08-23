package rtc

import (
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
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdp "github.com/livekit/protocol/sdp"

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
	negotiationFailedTimeout   = 15 * time.Second
	dtlsRetransmissionInterval = 100 * time.Millisecond

	iceDisconnectedTimeout = 10 * time.Second // compatible for ice-lite with firefox client
	iceFailedTimeout       = 25 * time.Second // pion's default
	iceKeepaliveInterval   = 2 * time.Second  // pion's default

	shortConnectionThreshold = 90 * time.Second
)

var (
	ErrIceRestartWithoutLocalSDP = errors.New("ICE restart without local SDP settled")
	ErrNoTransceiver             = errors.New("no transceiver")
	ErrNoSender                  = errors.New("no sender")
	ErrNoICECandidateHandler     = errors.New("no ICE candidate handler")
	ErrNoOfferHandler            = errors.New("no offer handler")
	ErrNoAnswerHandler           = errors.New("no answer handler")
)

// -------------------------------------------------------------------------

type signal int

const (
	signalICEGatheringComplete = iota
	signalLocalICECandidate
	signalRemoteICECandidate
	signalSendOffer
	signalRemoteDescriptionReceived
	signalICERestart
	signalPreviousAnswer
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
	case signalPreviousAnswer:
		return "PREVIOUS_ANSWER"
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

// -------------------------------------------------------

type event struct {
	signal signal
	data   interface{}
}

func (e event) String() string {
	return fmt.Sprintf("StreamAllocator:Event{signal: %s, data: %+v}", e.signal, e.data)
}

// -------------------------------------------------------

const (
	negotiationStateNone = iota
	// waiting for client answer
	negotiationStateClient
	// need to Negotiate again
	negotiationRetry
)

// -------------------------------------------------------

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

	debouncedNegotiate func(func())
	debouncePending    bool

	localDescriptionSent      bool
	cachedLocalCandidates     []*webrtc.ICECandidate
	pendingRemoteCandidates   []*webrtc.ICECandidateInit
	negotiationPending        map[livekit.ParticipantID]bool
	restartAfterGathering     bool
	restartAtNextOffer        bool
	negotiationState          int
	negotiateCounter          atomic.Int32
	signalStateCheckTimer     *time.Timer
	currentOfferIceCredential string // ice user:pwd, for publish side ice restart checking
	pendingRestartIceOffer    *webrtc.SessionDescription

	// for cleaner logging
	allowedLocalCandidates   []string
	allowedRemoteCandidates  []string
	filteredLocalCandidates  []string
	filteredRemoteCandidates []string

	onICECandidate      func(c *webrtc.ICECandidate) error
	onOffer             func(offer webrtc.SessionDescription) error
	onAnswer            func(offer webrtc.SessionDescription) error
	onInitialConnected  func()
	onFailed            func(isShortLived bool)
	onGetDTX            func() bool
	onNegotiationFailed func()

	// stream allocator for subscriber PC
	streamAllocator *sfu.StreamAllocator

	previousAnswer *webrtc.SessionDescription

	preferTCP bool

	eventChMu sync.RWMutex
	eventCh   chan event

	isClosed atomic.Bool
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
	ClientInfo              ClientInfo
}

func newPeerConnection(params TransportParams, onBandwidthEstimator func(estimator cc.BandwidthEstimator)) (*webrtc.PeerConnection, *webrtc.MediaEngine, error) {
	var directionConfig DirectionConfig
	if params.Target == livekit.SignalTarget_PUBLISHER {
		directionConfig = params.Config.Publisher
	} else {
		directionConfig = params.Config.Subscriber
	}

	// enable nack if audio red is not support
	if !isCodecEnabled(params.EnabledCodecs, webrtc.RTPCodecCapability{MimeType: sfu.MimeTypeAudioRed}) || !params.ClientInfo.SupportsAudioRED() {
		directionConfig.RTCPFeedback.Audio = append(directionConfig.RTCPFeedback.Audio, webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBNACK})
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
		eventCh:            make(chan event, 50),
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

	go t.processEvents()

	return t, nil
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

func (t *PCTransport) logICECandidates() {
	t.lock.RLock()
	t.params.Logger.Infow(
		"ice candidates",
		"lc", t.allowedLocalCandidates,
		"rc", t.allowedRemoteCandidates,
		"lc (filtered)", t.filteredLocalCandidates,
		"rc (filtered)", t.filteredRemoteCandidates,
	)
	t.lock.RUnlock()
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
	t.params.Logger.Infow("ice gathering state change", "state", state.String())
	if state != webrtc.ICEGathererStateComplete {
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
		t.logICECandidates()
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
		t.params.Logger.Debugw("reliable data channel open")
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
		t.params.Logger.Debugw("lossy data channel open")
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

func (t *PCTransport) AddICECandidate(candidate webrtc.ICECandidateInit) {
	t.postEvent(event{
		signal: signalRemoteICECandidate,
		data:   &candidate,
	})
}

func (t *PCTransport) PeerConnection() *webrtc.PeerConnection {
	return t.pc
}

func (t *PCTransport) AddTrack(trackLocal webrtc.TrackLocal) (sender *webrtc.RTPSender, transceiver *webrtc.RTPTransceiver, err error) {
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

	return
}

func (t *PCTransport) AddTransceiverFromTrack(trackLocal webrtc.TrackLocal) (sender *webrtc.RTPSender, transceiver *webrtc.RTPTransceiver, err error) {
	transceiver, err = t.pc.AddTransceiverFromTrack(trackLocal)
	if err != nil {
		return
	}

	sender = transceiver.Sender()
	if sender == nil {
		err = ErrNoSender
		return
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

	dc, err = t.pc.CreateDataChannel(dcLabel, dci)
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
	t.eventChMu.Lock()
	if t.isClosed.Swap(true) {
		t.eventChMu.Unlock()
		return
	}

	close(t.eventCh)
	t.eventChMu.Unlock()

	if t.streamAllocator != nil {
		t.streamAllocator.Stop()
	}

	_ = t.pc.Close()
}

func (t *PCTransport) HandleRemoteDescription(sd webrtc.SessionDescription) {
	t.postEvent(event{
		signal: signalRemoteDescriptionReceived,
		data:   &sd,
	})
}

func (t *PCTransport) OnICECandidate(f func(c *webrtc.ICECandidate) error) {
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

func (t *PCTransport) OnGetDTX(f func() bool) {
	t.onGetDTX = f
}

func (t *PCTransport) OnTrack(f func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver)) {
	t.pc.OnTrack(f)
}

func (t *PCTransport) OnDataPacket(f func(kind livekit.DataPacket_Kind, data []byte)) {
	t.onDataPacket = f
}

// OnOffer is called when the PeerConnection starts negotiation and prepares an offer
func (t *PCTransport) OnOffer(f func(sd webrtc.SessionDescription) error) {
	t.onOffer = f
}

func (t *PCTransport) OnAnswer(f func(sd webrtc.SessionDescription) error) {
	t.onAnswer = f
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
		t.lock.Lock()
		t.debouncedNegotiate(func() {
			// no op to cancel pending negotiation
		})
		t.debouncePending = false
		t.lock.Unlock()

		t.postEvent(event{
			signal: signalSendOffer,
		})
	} else {
		t.lock.Lock()
		if !t.debouncePending {
			t.debouncedNegotiate(func() {
				t.lock.Lock()
				t.debouncePending = false
				t.lock.Unlock()

				t.postEvent(event{
					signal: signalSendOffer,
				})
			})
			t.debouncePending = true
		}
		t.lock.Unlock()
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

func (t *PCTransport) clearSignalStateCheckTimer() {
	t.lock.Lock()
	t.clearSignalStateCheckTimerLocked()
	t.lock.Unlock()
}

func (t *PCTransport) clearSignalStateCheckTimerLocked() {
	if t.signalStateCheckTimer != nil {
		t.signalStateCheckTimer.Stop()
		t.signalStateCheckTimer = nil
	}
}

func (t *PCTransport) setupSignalStateCheckTimerLocked() {
	negotiateVersion := t.negotiateCounter.Inc()
	t.clearSignalStateCheckTimerLocked()
	t.signalStateCheckTimer = time.AfterFunc(negotiationFailedTimeout, func() {
		t.lock.Lock()
		t.clearSignalStateCheckTimerLocked()

		failed := t.negotiationState != negotiationStateNone
		t.lock.Unlock()

		if t.negotiateCounter.Load() == negotiateVersion && failed {
			if t.onNegotiationFailed != nil {
				t.onNegotiationFailed()
			}
		}
	})
}

func (t *PCTransport) ICERestart() {
	t.postEvent(event{
		signal: signalICERestart,
	})
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
	t.pc.SetLocalDescription(offer)

	//
	// Simulate client side peer connection and set DTLS role from previous answer.
	// Role needs to be set properly (one side needs to be server and the other side
	// needs to be the client) for DTLS connection to form properly. As this is
	// trying to replicate previous setup, read from previous answer and use that role.
	//
	se := webrtc.SettingEngine{}
	se.SetAnsweringDTLSRole(lksdp.ExtractDTLSRole(parsed))
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
		mid := lksdp.GetMidValue(m)
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

func (t *PCTransport) AddTrackToStreamAllocator(subTrack types.SubscribedTrack) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.AddTrack(subTrack.DownTrack(), sfu.AddTrackParams{
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

func (t *PCTransport) SetPreviousAnswer(answer *webrtc.SessionDescription) {
	t.postEvent(event{
		signal: signalPreviousAnswer,
		data:   answer,
	})
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

func (t *PCTransport) postEvent(event event) {
	t.eventChMu.RLock()
	if t.isClosed.Load() {
		t.eventChMu.RUnlock()
		return
	}

	select {
	case t.eventCh <- event:
	default:
		t.params.Logger.Warnw("event queue full", nil)
	}
	t.eventChMu.RUnlock()
}

func (t *PCTransport) processEvents() {
	for event := range t.eventCh {
		err := t.handleEvent(&event)
		if err != nil {
			t.params.Logger.Errorw("error handling event", err, "event", event.String())
			if t.onNegotiationFailed != nil {
				t.onNegotiationFailed()
			}
			break
		}
	}

	t.clearSignalStateCheckTimer()
	t.params.Logger.Infow("leaving events processor")
}

func (t *PCTransport) handleEvent(e *event) error {
	switch e.signal {
	case signalICEGatheringComplete:
		return t.handleICEGatheringComplete(e)
	case signalLocalICECandidate:
		return t.handleLocalICECandidate(e)
	case signalRemoteICECandidate:
		return t.handleRemoteICECandidate(e)
	case signalSendOffer:
		return t.handleSendOffer(e)
	case signalRemoteDescriptionReceived:
		return t.handleRemoteDescriptionReceived(e)
	case signalICERestart:
		return t.handleICERestart(e)
	case signalPreviousAnswer:
		return t.handlePreviousAnswer(e)
	}

	return nil
}

func (t *PCTransport) handleICEGatheringComplete(e *event) error {
	if t.params.Target == livekit.SignalTarget_SUBSCRIBER {
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
	return t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true})
}

func (t *PCTransport) handleICEGatheringCompleteAnswerer() error {
	if t.pendingRestartIceOffer == nil {
		return nil
	}

	t.params.Logger.Debugw("accept remote restart ice offer after ICE gathering")
	err := t.setRemoteDescription(*t.pendingRestartIceOffer)
	t.pendingRestartIceOffer = nil
	return err
}

func (t *PCTransport) handleLocalDescriptionSent() error {
	if t.localDescriptionSent {
		return nil
	}

	t.localDescriptionSent = true

	cachedLocalCandidates := t.cachedLocalCandidates
	t.cachedLocalCandidates = nil

	if t.onICECandidate != nil {
		for _, c := range cachedLocalCandidates {
			if err := t.onICECandidate(c); err != nil {
				return err
			}
		}

		return nil
	}

	return ErrNoICECandidateHandler
}

func (t *PCTransport) clearLocalDescriptionSent() {
	t.localDescriptionSent = false

	t.allowedLocalCandidates = nil
	t.allowedRemoteCandidates = nil
	t.filteredLocalCandidates = nil
	t.filteredRemoteCandidates = nil
}

func (t *PCTransport) handleLocalICECandidate(e *event) error {
	c := e.data.(*webrtc.ICECandidate)

	filtered := false
	t.lock.RLock()
	if t.preferTCP && c != nil && c.Protocol != webrtc.ICEProtocolTCP {
		cstr := c.String()
		t.params.Logger.Infow("filtering out local candidate", "candidate", cstr)
		t.filteredLocalCandidates = append(t.filteredLocalCandidates, cstr)
		filtered = true
	}
	t.lock.RUnlock()

	if filtered {
		return nil
	}

	if c != nil {
		t.allowedLocalCandidates = append(t.allowedLocalCandidates, c.String())
	}
	if !t.localDescriptionSent {
		t.cachedLocalCandidates = append(t.cachedLocalCandidates, c)
		return nil
	}

	if t.onICECandidate != nil {
		return t.onICECandidate(c)
	}

	return ErrNoICECandidateHandler
}

func (t *PCTransport) handleRemoteICECandidate(e *event) error {
	c := e.data.(*webrtc.ICECandidateInit)

	filtered := false
	t.lock.RLock()
	if t.preferTCP && !strings.Contains(c.Candidate, "tcp") {
		t.params.Logger.Infow("filtering out remote candidate", "candidate", c.Candidate)
		t.filteredRemoteCandidates = append(t.filteredRemoteCandidates, c.Candidate)
		filtered = true
	}
	t.lock.RUnlock()

	if filtered {
		return nil
	}

	if t.pc.RemoteDescription() == nil {
		t.pendingRemoteCandidates = append(t.pendingRemoteCandidates, c)
		return nil
	}

	t.allowedRemoteCandidates = append(t.allowedRemoteCandidates, c.Candidate)

	t.params.Logger.Infow("add candidate ", "candidate", c.Candidate)
	if err := t.pc.AddICECandidate(*c); err != nil {
		return errors.Wrap(err, "add ice candidate failed")
	}

	return nil
}

func (t *PCTransport) createAndSendOffer(options *webrtc.OfferOptions) error {
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		t.params.Logger.Warnw("trying to send offer on closed peer connection", nil)
		return nil
	}

	// when there's an ongoing negotiation, let it finish and not disrupt its state
	if t.negotiationState == negotiationStateClient {
		t.params.Logger.Infow("skipping negotiation, trying again later")
		t.negotiationState = negotiationRetry
		return nil
	} else if t.negotiationState == negotiationRetry {
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

	if t.previousAnswer != nil {
		t.previousAnswer = nil
		options = ensureICERestart(options)
	}

	if t.restartAtNextOffer {
		t.restartAtNextOffer = false
		options = ensureICERestart(options)
	}

	if options != nil && options.ICERestart {
		t.clearLocalDescriptionSent()
	}

	offer, err := t.pc.CreateOffer(options)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "create").Add(1)
		return errors.Wrap(err, "create offer failed")
	}

	t.lock.RLock()
	if t.preferTCP {
		t.params.Logger.Infow("local offer (unfiltered)", "sdp", offer.SDP)
	}
	t.lock.RUnlock()

	err = t.pc.SetLocalDescription(offer)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "local_description").Add(1)
		return errors.Wrap(err, "setting local description failed")
	}

	t.lock.RLock()
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
	t.lock.RUnlock()

	// indicate waiting for client
	t.negotiationState = negotiationStateClient
	t.negotiationPending = make(map[livekit.ParticipantID]bool)

	t.setupSignalStateCheckTimerLocked()

	if t.onOffer != nil {
		if err := t.onOffer(offer); err != nil {
			return errors.Wrap(err, "could not send offer")
		}

		return t.handleLocalDescriptionSent()
	}

	return ErrNoOfferHandler
}

func (t *PCTransport) handleSendOffer(e *event) error {
	return t.createAndSendOffer(nil)
}

func (t *PCTransport) handleRemoteDescriptionReceived(e *event) error {
	sd := e.data.(*webrtc.SessionDescription)
	if sd.Type == webrtc.SDPTypeOffer {
		return t.handleRemoteOfferReceived(sd)
	} else {
		return t.handleRemoteAnswerReceived(sd)
	}
}

func (t *PCTransport) isRemoteOfferRestartICE(sd *webrtc.SessionDescription) (string, bool, error) {
	parsed, err := sd.Unmarshal()
	if err != nil {
		return "", false, err
	}
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
	t.lock.RLock()
	if t.preferTCP {
		t.params.Logger.Infow("remote description (unfiltered)", "type", sd.Type, "sdp", sd.SDP)
	}
	sd = t.filterCandidates(sd)
	if t.preferTCP {
		t.params.Logger.Infow("remote description (filtered)", "type", sd.Type, "sdp", sd.SDP)
	}
	t.lock.RUnlock()

	if err := t.pc.SetRemoteDescription(sd); err != nil {
		return errors.Wrap(err, "setting remote description failed")
	}

	for _, c := range t.pendingRemoteCandidates {
		if err := t.pc.AddICECandidate(*c); err != nil {
			return errors.Wrap(err, "add ice candidate failed")
		}
	}
	t.pendingRemoteCandidates = nil

	return nil
}

func (t *PCTransport) createAndSendAnswer() error {
	enableDTX := false
	if t.onGetDTX != nil {
		enableDTX = t.onGetDTX()
	}
	t.configureReceiverDTX(enableDTX)

	answer, err := t.pc.CreateAnswer(nil)
	if err != nil {
		return errors.Wrap(err, "create answer failed")
	}

	t.lock.RLock()
	if t.preferTCP {
		t.params.Logger.Infow("local answer (unfiltered)", "sdp", answer.SDP)
	}
	t.lock.RUnlock()

	if err = t.pc.SetLocalDescription(answer); err != nil {
		return errors.Wrap(err, "setting local description failed")
	}

	//
	// Filter after setting local description as pion expects the answer
	// to match between CreateAnswer and SetLocalDescription.
	// Filtered answer is sent to remote so that remote does not
	// see filtered candidates.
	//
	t.lock.RLock()
	answer = t.filterCandidates(answer)
	if t.preferTCP {
		t.params.Logger.Infow("local answer (filtered)", "sdp", answer.SDP)
	}
	t.lock.RUnlock()

	if t.onAnswer != nil {
		if err := t.onAnswer(answer); err != nil {
			return errors.Wrap(err, "could not send answer")
		}

		return t.handleLocalDescriptionSent()
	}

	return ErrNoAnswerHandler
}

func (t *PCTransport) handleRemoteOfferReceived(sd *webrtc.SessionDescription) error {
	iceCredential, offerRestartICE, err := t.isRemoteOfferRestartICE(sd)
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

	if err := t.setRemoteDescription(*sd); err != nil {
		return err
	}

	if t.currentOfferIceCredential == "" || offerRestartICE {
		t.currentOfferIceCredential = iceCredential
	}

	return t.createAndSendAnswer()
}

func (t *PCTransport) handleRemoteAnswerReceived(sd *webrtc.SessionDescription) error {
	if err := t.setRemoteDescription(*sd); err != nil {
		return err
	}

	t.clearSignalStateCheckTimerLocked()

	if t.negotiationState == negotiationRetry {
		t.negotiationState = negotiationStateNone
		t.params.Logger.Debugw("re-negotiate after receiving answer")
		return t.createAndSendOffer(nil)
	}

	t.negotiationState = negotiationStateNone
	return nil
}

func (t *PCTransport) handleICERestart(e *event) error {
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		t.params.Logger.Warnw("trying to restart ICE on closed peer connection", nil)
		return nil
	}

	// if restart is requested, and we are not ready, then continue afterwards
	if t.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
		t.params.Logger.Debugw("restart ICE after gathering")
		t.restartAfterGathering = true
		return nil
	}

	if t.negotiationState == negotiationStateNone {
		return t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true})
	}

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
			if t.onOffer != nil {
				return t.onOffer(*offer)
			}
			return ErrNoOfferHandler
		}
	} else {
		// recover by re-applying the last answer
		t.params.Logger.Infow("recovering from client negotiation state on ICE restart")
		if err := t.pc.SetRemoteDescription(*currentSD); err != nil {
			prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "remote_description").Add(1)
			return errors.Wrap(err, "set remote description failed")
		} else {
			return t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true})
		}
	}
}

func (t *PCTransport) handlePreviousAnswer(e *event) error {
	answer := e.data.(*webrtc.SessionDescription)
	if t.pc.RemoteDescription() == nil && t.previousAnswer == nil {
		t.previousAnswer = answer
		if err := t.initPCWithPreviousAnswer(*t.previousAnswer); err != nil {
			return errors.Wrap(err, "initPCWithPreviousAnswer failed")
		}
	}

	return nil
}
