package rtc

import (
	"math/bits"
	"strings"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	failureCountThreshold = 2

	// when RR report loss percentage over this threshold, we consider it is a unstable event
	udpLossFracUnstable = 25
	// if in last 32 times RR, the unstable report count over this threshold, the connection is unstable
	udpLossUnstableCountThreshold = 20
	tcpGoodRTT                    = 80
)

type TransportManagerParams struct {
	Identity                livekit.ParticipantIdentity
	SID                     livekit.ParticipantID
	SubscriberAsPrimary     bool
	Config                  *WebRTCConfig
	ProtocolVersion         types.ProtocolVersion
	Telemetry               telemetry.TelemetryService
	CongestionControlConfig config.CongestionControlConfig
	EnabledCodecs           []*livekit.Codec
	SimTracks               map[uint32]SimulcastTrackInfo
	ClientConf              *livekit.ClientConfiguration
	ClientInfo              ClientInfo
	Migration               bool
	AllowTCPFallback        bool
	TURNSEnabled            bool
	Logger                  logger.Logger
}

type TransportManager struct {
	params TransportManagerParams

	lock sync.RWMutex

	publisher               *PCTransport
	subscriber              *PCTransport
	failureCount            int
	isTransportReconfigured bool

	pendingOfferPublisher        *webrtc.SessionDescription
	pendingDataChannelsPublisher []*livekit.DataChannelInfo
	lastPublisherAnswer          atomic.Value
	lastPublisherOffer           atomic.Value
	iceConfig                    types.IceConfig

	mediaLossProxy       *MediaLossProxy
	udpLossUnstableCount uint32
	tcpRTT, udpRTT       uint32

	onPublisherInitialConnected        func()
	onSubscriberInitialConnected       func()
	onPrimaryTransportInitialConnected func()
	onAnyTransportFailed               func()

	onICEConfigChanged func(iceConfig types.IceConfig)
}

func NewTransportManager(params TransportManagerParams) (*TransportManager, error) {
	t := &TransportManager{
		params:         params,
		mediaLossProxy: NewMediaLossProxy(MediaLossProxyParams{Logger: params.Logger}),
	}
	t.mediaLossProxy.OnMediaLossUpdate(t.onMediaLossUpdate)

	enabledCodecs := make([]*livekit.Codec, 0, len(params.EnabledCodecs))
	for _, c := range params.EnabledCodecs {
		var disabled bool
		for _, disableCodec := range params.ClientConf.GetDisabledCodecs().GetCodecs() {
			// disable codec's fmtp is empty means disable this codec entirely
			if strings.EqualFold(c.Mime, disableCodec.Mime) && (disableCodec.FmtpLine == "" || disableCodec.FmtpLine == c.FmtpLine) {
				disabled = true
				break
			}
		}
		if !disabled {
			enabledCodecs = append(enabledCodecs, c)
		}
	}

	publisher, err := NewPCTransport(TransportParams{
		ParticipantID:           params.SID,
		ParticipantIdentity:     params.Identity,
		ProtocolVersion:         params.ProtocolVersion,
		Config:                  params.Config,
		DirectionConfig:         params.Config.Publisher,
		CongestionControlConfig: params.CongestionControlConfig,
		Telemetry:               params.Telemetry,
		EnabledCodecs:           enabledCodecs,
		Logger:                  LoggerWithPCTarget(params.Logger, livekit.SignalTarget_PUBLISHER),
		SimTracks:               params.SimTracks,
		ClientInfo:              params.ClientInfo,
	})
	if err != nil {
		return nil, err
	}
	t.publisher = publisher
	t.publisher.OnInitialConnected(func() {
		if t.onPublisherInitialConnected != nil {
			t.onPublisherInitialConnected()
		}
		if !t.params.SubscriberAsPrimary && t.onPrimaryTransportInitialConnected != nil {
			t.onPrimaryTransportInitialConnected()
		}
	})
	t.publisher.OnFailed(func(isShortLived bool) {
		t.handleConnectionFailed(isShortLived)
		if t.onAnyTransportFailed != nil {
			t.onAnyTransportFailed()
		}
	})

	subscriber, err := NewPCTransport(TransportParams{
		ParticipantID:           params.SID,
		ParticipantIdentity:     params.Identity,
		ProtocolVersion:         params.ProtocolVersion,
		Config:                  params.Config,
		DirectionConfig:         params.Config.Subscriber,
		CongestionControlConfig: params.CongestionControlConfig,
		Telemetry:               params.Telemetry,
		EnabledCodecs:           enabledCodecs,
		Logger:                  LoggerWithPCTarget(params.Logger, livekit.SignalTarget_SUBSCRIBER),
		ClientInfo:              params.ClientInfo,
		IsOfferer:               true,
		IsSendSide:              true,
	})
	if err != nil {
		return nil, err
	}
	t.subscriber = subscriber
	t.subscriber.OnInitialConnected(func() {
		if t.onSubscriberInitialConnected != nil {
			t.onSubscriberInitialConnected()
		}
		if t.params.SubscriberAsPrimary && t.onPrimaryTransportInitialConnected != nil {
			t.onPrimaryTransportInitialConnected()
		}
	})
	t.subscriber.OnFailed(func(isShortLived bool) {
		t.handleConnectionFailed(isShortLived)
		if t.onAnyTransportFailed != nil {
			t.onAnyTransportFailed()
		}
	})
	if !t.params.Migration {
		if err := t.createDataChannelsForSubscriber(nil); err != nil {
			return nil, err
		}
	}

	return t, nil
}

func (t *TransportManager) Close() {
	t.publisher.Close()
	t.subscriber.Close()
}

func (t *TransportManager) HaveAllTransportEverConnected() bool {
	return t.publisher.HasEverConnected() && t.subscriber.HasEverConnected()
}

func (t *TransportManager) SubscriberClose() {
	t.subscriber.Close()
}

func (t *TransportManager) OnPublisherICECandidate(f func(c *webrtc.ICECandidate) error) {
	t.publisher.OnICECandidate(f)
}

func (t *TransportManager) OnPublisherAnswer(f func(answer webrtc.SessionDescription) error) {
	t.publisher.OnAnswer(func(sd webrtc.SessionDescription) error {
		t.lastPublisherAnswer.Store(sd)
		return f(sd)
	})
}

func (t *TransportManager) OnPublisherInitialConnected(f func()) {
	t.onPublisherInitialConnected = f
}

func (t *TransportManager) OnPublisherTrack(f func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver)) {
	t.publisher.OnTrack(f)
}

func (t *TransportManager) IsPublisherEstablished() bool {
	return t.publisher.IsEstablished()
}

func (t *TransportManager) GetPublisherMid(rtpReceiver *webrtc.RTPReceiver) string {
	return t.publisher.GetMid(rtpReceiver)
}

func (t *TransportManager) GetPublisherRTPReceiver(mid string) *webrtc.RTPReceiver {
	return t.publisher.GetRTPReceiver(mid)
}

func (t *TransportManager) WritePublisherRTCP(pkts []rtcp.Packet) error {
	return t.publisher.WriteRTCP(pkts)
}

func (t *TransportManager) OnSubscriberICECandidate(f func(c *webrtc.ICECandidate) error) {
	t.subscriber.OnICECandidate(f)
}

func (t *TransportManager) OnSubscriberOffer(f func(offer webrtc.SessionDescription) error) {
	t.subscriber.OnOffer(f)
}

func (t *TransportManager) OnSubscriberInitialConnected(f func()) {
	t.onSubscriberInitialConnected = f
}

func (t *TransportManager) OnSubscriberStreamStateChange(f func(update *sfu.StreamStateUpdate) error) {
	t.subscriber.OnStreamStateChange(f)
}

func (t *TransportManager) HasSubscriberEverConnected() bool {
	return t.subscriber.HasEverConnected()
}

func (t *TransportManager) AddTrackToSubscriber(trackLocal webrtc.TrackLocal, params types.AddTrackParams) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	return t.subscriber.AddTrack(trackLocal, params)
}

func (t *TransportManager) AddTransceiverFromTrackToSubscriber(trackLocal webrtc.TrackLocal, params types.AddTrackParams) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	return t.subscriber.AddTransceiverFromTrack(trackLocal, params)
}

func (t *TransportManager) RemoveTrackFromSubscriber(sender *webrtc.RTPSender) error {
	return t.subscriber.RemoveTrack(sender)
}

func (t *TransportManager) WriteSubscriberRTCP(pkts []rtcp.Packet) error {
	return t.subscriber.WriteRTCP(pkts)
}

func (t *TransportManager) OnPrimaryTransportInitialConnected(f func()) {
	t.onPrimaryTransportInitialConnected = f
}

func (t *TransportManager) OnPrimaryTransportFullyEstablished(f func()) {
	t.getTransport(true).OnFullyEstablished(f)
}

func (t *TransportManager) OnAnyTransportFailed(f func()) {
	t.onAnyTransportFailed = f
}

func (t *TransportManager) OnAnyTransportNegotiationFailed(f func()) {
	t.publisher.OnNegotiationFailed(f)
	t.subscriber.OnNegotiationFailed(f)
}

func (t *TransportManager) AddSubscribedTrack(subTrack types.SubscribedTrack) {
	t.subscriber.AddTrackToStreamAllocator(subTrack)
}

func (t *TransportManager) RemoveSubscribedTrack(subTrack types.SubscribedTrack) {
	t.subscriber.RemoveTrackFromStreamAllocator(subTrack)
}

func (t *TransportManager) OnDataMessage(f func(kind livekit.DataPacket_Kind, data []byte)) {
	// upstream data is always comes in via publisher peer connection irrespective of which is primary
	t.publisher.OnDataPacket(f)
}

func (t *TransportManager) SendDataPacket(dp *livekit.DataPacket) error {
	// downstream data is sent via primary peer connection
	return t.getTransport(true).SendDataPacket(dp)
}

func (t *TransportManager) createDataChannelsForSubscriber(pendingDataChannels []*livekit.DataChannelInfo) error {
	var (
		reliableID, lossyID       uint16
		reliableIDPtr, lossyIDPtr *uint16
	)

	//
	// For old version migration clients, they don't send subscriber data channel info
	// so we need to create data channels with default ID and don't negotiate as client already has
	// data channels with default ID.
	//
	// For new version migration clients, we create data channels with new ID and negotiate with client
	//
	for _, dc := range pendingDataChannels {
		if dc.Label == ReliableDataChannel {
			// pion use step 2 for auto generated ID, so we need to add 4 to avoid conflict
			reliableID = uint16(dc.Id) + 4
			reliableIDPtr = &reliableID
		} else if dc.Label == LossyDataChannel {
			lossyID = uint16(dc.Id) + 4
			lossyIDPtr = &lossyID
		}
	}

	ordered := true
	negotiated := t.params.Migration && reliableIDPtr == nil
	if err := t.subscriber.CreateDataChannel(ReliableDataChannel, &webrtc.DataChannelInit{
		Ordered:    &ordered,
		ID:         reliableIDPtr,
		Negotiated: &negotiated,
	}); err != nil {
		return err
	}

	retransmits := uint16(0)
	negotiated = t.params.Migration && lossyIDPtr == nil
	if err := t.subscriber.CreateDataChannel(LossyDataChannel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &retransmits,
		ID:             lossyIDPtr,
		Negotiated:     &negotiated,
	}); err != nil {
		return err
	}
	return nil
}

func (t *TransportManager) GetUnmatchMediaForOffer(offer webrtc.SessionDescription, mediaType string) (parsed *sdp.SessionDescription, unmatched []*sdp.MediaDescription, err error) {
	// prefer codec from offer for clients that don't support setCodecPreferences
	parsed, err = offer.Unmarshal()
	if err != nil {
		t.params.Logger.Errorw("failed to parse offer for codec preference", err)
		return
	}

	var lastMatchedMid string
	lastAnswer := t.lastPublisherAnswer.Load()
	if lastAnswer != nil {
		answer := lastAnswer.(webrtc.SessionDescription)
		parsedAnswer, err1 := answer.Unmarshal()
		if err1 != nil {
			// should not happend
			t.params.Logger.Errorw("failed to parse last answer", err)
			return
		}

		for i := len(parsedAnswer.MediaDescriptions) - 1; i >= 0; i-- {
			media := parsedAnswer.MediaDescriptions[i]
			if media.MediaName.Media == mediaType {
				lastMatchedMid, _ = media.Attribute(sdp.AttrKeyMID)
				break
			}
		}
	}

	for i := len(parsed.MediaDescriptions) - 1; i >= 0; i-- {
		media := parsed.MediaDescriptions[i]
		if media.MediaName.Media == mediaType {
			mid, _ := media.Attribute(sdp.AttrKeyMID)
			if mid == lastMatchedMid {
				break
			}
			unmatched = append(unmatched, media)
		}
	}

	return
}

func (t *TransportManager) LastPublisherOffer() webrtc.SessionDescription {
	if sd := t.lastPublisherOffer.Load(); sd != nil {
		return sd.(webrtc.SessionDescription)
	}
	return webrtc.SessionDescription{}
}

func (t *TransportManager) HandleOffer(offer webrtc.SessionDescription, shouldPend bool) {
	t.lock.Lock()
	if shouldPend {
		t.pendingOfferPublisher = &offer
		t.lock.Unlock()
		return
	}
	t.lock.Unlock()
	t.lastPublisherOffer.Store(offer)

	t.publisher.HandleRemoteDescription(offer)
}

func (t *TransportManager) ProcessPendingPublisherOffer() {
	t.lock.Lock()
	pendingOffer := t.pendingOfferPublisher
	t.pendingOfferPublisher = nil
	t.lock.Unlock()

	if pendingOffer != nil {
		t.HandleOffer(*pendingOffer, false)
	}
}

func (t *TransportManager) HandleAnswer(answer webrtc.SessionDescription) {
	t.subscriber.HandleRemoteDescription(answer)
}

// AddICECandidate adds candidates for remote peer
func (t *TransportManager) AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) {
	switch target {
	case livekit.SignalTarget_PUBLISHER:
		t.publisher.AddICECandidate(candidate)
	case livekit.SignalTarget_SUBSCRIBER:
		t.subscriber.AddICECandidate(candidate)
	default:
		err := errors.New("unknown signal target")
		t.params.Logger.Errorw("ice candidate for unknown signal target", err, "target", target)
	}
}

func (t *TransportManager) NegotiateSubscriber(force bool) {
	t.subscriber.Negotiate(force)
}

func (t *TransportManager) ICERestart(iceConfig *types.IceConfig) {
	if iceConfig != nil {
		t.SetICEConfig(*iceConfig)
	}

	t.subscriber.ICERestart()
}

func (t *TransportManager) OnICEConfigChanged(f func(iceConfig types.IceConfig)) {
	t.lock.Lock()
	t.onICEConfigChanged = f
	t.lock.Unlock()
}

func (t *TransportManager) SetICEConfig(iceConfig types.IceConfig) {
	t.configureICE(iceConfig, true)
}

func (t *TransportManager) configureICE(iceConfig types.IceConfig, reset bool) {
	t.lock.Lock()
	if t.iceConfig == iceConfig {
		t.lock.Unlock()
		return
	}

	t.params.Logger.Infow("setting ICE config", "iceConfig", iceConfig)
	onICEConfigChanged := t.onICEConfigChanged
	t.iceConfig = iceConfig
	t.failureCount = 0
	t.isTransportReconfigured = !reset
	t.lock.Unlock()

	if iceConfig.PreferSub != types.PreferNone {
		t.mediaLossProxy.OnMediaLossUpdate(nil)
	}

	t.publisher.SetPreferTCP(iceConfig.PreferPub == types.PreferTcp)
	t.subscriber.SetPreferTCP(iceConfig.PreferSub == types.PreferTcp)

	if onICEConfigChanged != nil {
		onICEConfigChanged(iceConfig)
	}
}

func (t *TransportManager) SubscriberAsPrimary() bool {
	return t.params.SubscriberAsPrimary
}

func (t *TransportManager) GetICEConnectionType() types.ICEConnectionType {
	return t.getTransport(true).GetICEConnectionType()
}

func (t *TransportManager) getTransport(isPrimary bool) *PCTransport {
	pcTransport := t.publisher
	if (isPrimary && t.params.SubscriberAsPrimary) || (!isPrimary && !t.params.SubscriberAsPrimary) {
		pcTransport = t.subscriber
	}

	return pcTransport
}

func (t *TransportManager) handleConnectionFailed(isShortLived bool) {
	if !t.params.AllowTCPFallback {
		return
	}

	t.lock.Lock()
	if t.isTransportReconfigured {
		t.lock.Unlock()
		return
	}

	//
	// Checking only `PreferSub` field although any connection failure (PUBLISHER OR SUBSCRIBER) will
	// flow through here.
	//
	// As both transports are switched to the same type on any failure, checking just subscriber should be fine.
	//
	getNext := func(ic types.IceConfig) types.PreferCandidateType {
		if ic.PreferSub == types.PreferNone && t.params.ClientInfo.SupportsICETCP() {
			return types.PreferTcp
		} else if ic.PreferSub != types.PreferTls && t.params.TURNSEnabled {
			return types.PreferTls
		} else {
			return types.PreferNone
		}
	}

	var preferNext types.PreferCandidateType
	if isShortLived {
		preferNext = getNext(t.iceConfig)
	} else {
		t.failureCount++
		if t.failureCount < failureCountThreshold {
			t.lock.Unlock()
			return
		}

		preferNext = getNext(t.iceConfig)
	}

	if preferNext == t.iceConfig.PreferSub {
		t.lock.Unlock()
		return
	}

	t.isTransportReconfigured = true
	t.lock.Unlock()

	switch preferNext {
	case types.PreferTcp:
		t.params.Logger.Infow("prefer TCP transport on both peer connections")

	case types.PreferTls:
		t.params.Logger.Infow("prefer TLS transport both peer connections")

	case types.PreferNone:
		t.params.Logger.Infow("allowing all transports on both peer connections")
	}

	// irrespective of which one fails, force prefer candidate on both as the other one might
	// fail at a different time and cause another disruption
	t.configureICE(types.IceConfig{
		PreferPub: preferNext,
		PreferSub: preferNext,
	}, false)
}

func (t *TransportManager) SetMigrateInfo(previousOffer, previousAnswer *webrtc.SessionDescription, dataChannels []*livekit.DataChannelInfo) {
	t.lock.Lock()
	t.pendingDataChannelsPublisher = make([]*livekit.DataChannelInfo, 0, len(dataChannels))
	pendingDataChannelsSubscriber := make([]*livekit.DataChannelInfo, 0, len(dataChannels))
	for _, dci := range dataChannels {
		if dci.Target == livekit.SignalTarget_SUBSCRIBER {
			pendingDataChannelsSubscriber = append(pendingDataChannelsSubscriber, dci)
		} else {
			t.pendingDataChannelsPublisher = append(t.pendingDataChannelsPublisher, dci)
		}
	}
	t.lock.Unlock()

	if t.params.SubscriberAsPrimary {
		if err := t.createDataChannelsForSubscriber(pendingDataChannelsSubscriber); err != nil {
			t.params.Logger.Errorw("create subscriber data channels during migration failed", err)
		}
	}

	t.subscriber.SetPreviousSdp(previousOffer, previousAnswer)
}

func (t *TransportManager) ProcessPendingPublisherDataChannels() {
	t.lock.Lock()
	pendingDataChannels := t.pendingDataChannelsPublisher
	t.pendingDataChannelsPublisher = nil
	t.lock.Unlock()

	ordered := true
	negotiated := true

	for _, ci := range pendingDataChannels {
		var (
			dcLabel    string
			dcID       uint16
			dcExisting bool
			err        error
		)
		if ci.Label == LossyDataChannel {
			retransmits := uint16(0)
			id := uint16(ci.GetId())
			dcLabel, dcID, dcExisting, err = t.publisher.CreateDataChannelIfEmpty(LossyDataChannel, &webrtc.DataChannelInit{
				Ordered:        &ordered,
				MaxRetransmits: &retransmits,
				Negotiated:     &negotiated,
				ID:             &id,
			})
		} else if ci.Label == ReliableDataChannel {
			id := uint16(ci.GetId())
			dcLabel, dcID, dcExisting, err = t.publisher.CreateDataChannelIfEmpty(ReliableDataChannel, &webrtc.DataChannelInit{
				Ordered:    &ordered,
				Negotiated: &negotiated,
				ID:         &id,
			})
		}
		if err != nil {
			t.params.Logger.Errorw("create migrated data channel failed", err, "label", ci.Label)
		} else if dcExisting {
			t.params.Logger.Debugw("existing data channel during migration", "label", dcLabel, "id", dcID)
		} else {
			t.params.Logger.Debugw("create migrated data channel", "label", dcLabel, "id", dcID)
		}
	}
}

func (t *TransportManager) OnReceiverReport(dt *sfu.DownTrack, report *rtcp.ReceiverReport) {
	t.mediaLossProxy.HandleMaxLossFeedback(dt, report)
}

func (t *TransportManager) onMediaLossUpdate(loss uint8) {
	t.lock.Lock()
	t.udpLossUnstableCount <<= 1
	if loss >= uint8(255*udpLossFracUnstable/100) {
		t.udpLossUnstableCount |= 1
		if bits.OnesCount32(t.udpLossUnstableCount) >= udpLossUnstableCountThreshold {
			if t.udpRTT > 0 && t.tcpRTT < uint32(float32(t.udpRTT)*1.3) && t.tcpRTT < tcpGoodRTT {
				t.udpLossUnstableCount = 0
				t.lock.Unlock()
				t.params.Logger.Infow("udp connection unstable, switch to tcp")
				t.handleConnectionFailed(true)
				if t.onAnyTransportFailed != nil {
					t.onAnyTransportFailed()
				}
				return
			}
		}
	}
	t.lock.Unlock()
}

func (t *TransportManager) UpdateRTT(rtt uint32, isUDP bool) {
	if isUDP {
		if t.udpRTT == 0 {
			t.udpRTT = rtt
		} else {
			t.udpRTT = uint32(int(t.udpRTT) + (int(rtt)-int(t.udpRTT))/2)
		}
	} else {
		t.tcpRTT = rtt
	}
}
