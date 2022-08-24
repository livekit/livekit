package rtc

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
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
	Logger                  logger.Logger
}

type TransportManager struct {
	params TransportManagerParams

	publisher  *PCTransport
	subscriber *PCTransport

	lock sync.RWMutex

	pendingOfferPublisher        *webrtc.SessionDescription
	pendingDataChannelsPublisher []*livekit.DataChannelInfo
	lastPublisherAnswer          atomic.Value
	iceConfig                    types.IceConfig

	onPublisherGetDTX func() bool

	onPublisherInitialConnected        func()
	onSubscriberInitialConnected       func()
	onPrimaryTransportInitialConnected func()
	onAnyTransportFailed               func()

	onICEConfigChanged func(iceConfig types.IceConfig)
}

func NewTransportManager(params TransportManagerParams) (*TransportManager, error) {
	t := &TransportManager{
		params: params,
	}

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
		Target:                  livekit.SignalTarget_PUBLISHER,
		Config:                  params.Config,
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
	t.publisher.OnRemoteDescriptionSettled(t.createPublisherAnswerAndSend)
	t.publisher.OnInitialConnected(func() {
		if t.onPublisherInitialConnected != nil {
			t.onPublisherInitialConnected()
		}
		if !t.params.SubscriberAsPrimary && t.onPrimaryTransportInitialConnected != nil {
			t.onPrimaryTransportInitialConnected()
		}
	})
	t.publisher.OnFailed(func(isShortLived bool) {
		if t.onAnyTransportFailed != nil {
			t.onAnyTransportFailed()
		}
	})

	subscriber, err := NewPCTransport(TransportParams{
		ParticipantID:           params.SID,
		ParticipantIdentity:     params.Identity,
		ProtocolVersion:         params.ProtocolVersion,
		Target:                  livekit.SignalTarget_SUBSCRIBER,
		Config:                  params.Config,
		CongestionControlConfig: params.CongestionControlConfig,
		Telemetry:               params.Telemetry,
		EnabledCodecs:           enabledCodecs,
		Logger:                  LoggerWithPCTarget(params.Logger, livekit.SignalTarget_SUBSCRIBER),
		ClientInfo:              params.ClientInfo,
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

func (t *TransportManager) OnPublisherICECandidate(f func(c *webrtc.ICECandidate)) {
	t.publisher.OnICECandidate(f)
}

func (t *TransportManager) OnPublisherGetDTX(f func() bool) {
	t.onPublisherGetDTX = f
}

func (t *TransportManager) OnPublisherAnswer(f func(answer webrtc.SessionDescription)) {
	t.publisher.OnAnswer(func(sd webrtc.SessionDescription) {
		t.lastPublisherAnswer.Store(sd)
		f(sd)
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

func (t *TransportManager) PublisherLocalDescriptionSent() {
	t.publisher.LocalDescriptionSent()
}

func (t *TransportManager) WritePublisherRTCP(pkts []rtcp.Packet) error {
	return t.publisher.WriteRTCP(pkts)
}

func (t *TransportManager) OnSubscriberICECandidate(f func(c *webrtc.ICECandidate)) {
	t.subscriber.OnICECandidate(f)
}

func (t *TransportManager) OnSubscriberOffer(f func(offer webrtc.SessionDescription)) {
	t.subscriber.OnOffer(f)
}

func (t *TransportManager) OnSubscriberInitialConnected(f func()) {
	t.onSubscriberInitialConnected = f
}

func (t *TransportManager) OnSubscriberNegotiationFailed(f func()) {
	t.subscriber.OnNegotiationFailed(f)
}

func (t *TransportManager) OnSubscriberStreamStateChange(f func(update *sfu.StreamStateUpdate) error) {
	t.subscriber.OnStreamStateChange(f)
}

func (t *TransportManager) SubscriberLocalDescriptionSent() {
	t.subscriber.LocalDescriptionSent()
}

func (t *TransportManager) HasSubscriberEverConnected() bool {
	return t.subscriber.HasEverConnected()
}

func (t *TransportManager) AddTrackToSubscriber(trackLocal webrtc.TrackLocal) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	return t.subscriber.AddTrack(trackLocal)
}

func (t *TransportManager) AddTransceiverFromTrackToSubscriber(trackLocal webrtc.TrackLocal) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	return t.subscriber.AddTransceiverFromTrack(trackLocal)
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

func (t *TransportManager) GetLastUnmatchedMediaForOffer(offer webrtc.SessionDescription, mediaType string) (parsed *sdp.SessionDescription, unmatched *sdp.MediaDescription, err error) {
	// prefer codec from offer for clients that don't support setCodecPreferences
	parsed, err = offer.Unmarshal()
	if err != nil {
		t.params.Logger.Errorw("failed to parse offer for codec preference", err)
		return
	}

	for i := len(parsed.MediaDescriptions) - 1; i >= 0; i-- {
		media := parsed.MediaDescriptions[i]
		if media.MediaName.Media == mediaType {
			unmatched = media
			break
		}
	}

	if unmatched == nil {
		return
	}

	lastAnswer := t.lastPublisherAnswer.Load()
	if lastAnswer != nil {
		answer := lastAnswer.(webrtc.SessionDescription)
		parsedAnswer, err1 := answer.Unmarshal()
		if err1 != nil {
			// should not happend
			t.params.Logger.Errorw("failed to parse last answer", err)
			return
		}

		for _, m := range parsedAnswer.MediaDescriptions {
			mid, _ := m.Attribute(sdp.AttrKeyMID)
			if lastMid, _ := unmatched.Attribute(sdp.AttrKeyMID); lastMid == mid {
				// mid matched, return
				unmatched = nil
				return
			}
		}
	}

	return
}

func (t *TransportManager) HandleOffer(offer webrtc.SessionDescription, shouldPend bool) error {
	t.lock.Lock()
	if shouldPend {
		t.pendingOfferPublisher = &offer
		t.lock.Unlock()
		return nil
	}
	t.lock.Unlock()

	return t.publisher.SetRemoteDescription(offer)
}

func (t *TransportManager) ProcessPendingPublisherOffer() error {
	t.lock.Lock()
	pendingOffer := t.pendingOfferPublisher
	t.pendingOfferPublisher = nil
	t.lock.Unlock()

	if pendingOffer != nil {
		return t.HandleOffer(*pendingOffer, false)
	}

	return nil
}

func (t *TransportManager) createPublisherAnswerAndSend() error {
	enableDTX := false
	if t.onPublisherGetDTX != nil {
		enableDTX = t.onPublisherGetDTX()
	}
	err := t.publisher.CreateAndSendAnswer(enableDTX)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "create").Add(1)
		return errors.Wrap(err, "could not create answer")
	}

	return nil
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (t *TransportManager) HandleAnswer(answer webrtc.SessionDescription) error {
	if answer.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}

	t.params.Logger.Infow("received answer", "transport", livekit.SignalTarget_SUBSCRIBER)
	if err := t.subscriber.SetRemoteDescription(answer); err != nil {
		return errors.Wrap(err, "could not set answer")
	}

	return nil
}

// AddICECandidate adds candidates for remote peer
func (t *TransportManager) AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) error {
	switch target {
	case livekit.SignalTarget_PUBLISHER:
		return t.publisher.AddICECandidate(candidate)
	case livekit.SignalTarget_SUBSCRIBER:
		return t.subscriber.AddICECandidate(candidate)
	default:
		err := errors.New("unknown signal target")
		t.params.Logger.Errorw("ice candidate for unknown signal target", err, "target", target)
		return err
	}
}

func (t *TransportManager) NegotiateSubscriber(force bool) {
	t.subscriber.Negotiate(force)
}

func (t *TransportManager) AddNegotiationPending(publisherID livekit.ParticipantID) {
	t.subscriber.AddNegotiationPending(publisherID)
}

func (t *TransportManager) IsNegotiationPending(publisherID livekit.ParticipantID) bool {
	return t.subscriber.IsNegotiationPending(publisherID)
}

func (t *TransportManager) ICERestart(iceConfig *types.IceConfig) error {
	if iceConfig != nil {
		t.SetICEConfig(*iceConfig)
	}

	return t.subscriber.CreateAndSendOffer(&webrtc.OfferOptions{
		ICERestart: true,
	})
}

func (t *TransportManager) OnICEConfigChanged(f func(iceConfig types.IceConfig)) {
	t.lock.Lock()
	t.onICEConfigChanged = f
	t.lock.Unlock()
}

func (t *TransportManager) SetICEConfig(iceConfig types.IceConfig) {
	t.params.Logger.Infow("setting ICE config", "iceConfig", iceConfig)

	t.publisher.SetPreferTCP(iceConfig.PreferPub == types.PreferTcp)
	t.subscriber.SetPreferTCP(iceConfig.PreferSub == types.PreferTcp)

	t.lock.Lock()
	onICEConfigChanged := t.onICEConfigChanged
	t.iceConfig = iceConfig
	t.lock.Unlock()

	if onICEConfigChanged != nil {
		onICEConfigChanged(iceConfig)
	}
}

func (t *TransportManager) SubscriberAsPrimary() bool {
	return t.params.SubscriberAsPrimary
}

func (t *TransportManager) getTransport(isPrimary bool) *PCTransport {
	pcTransport := t.publisher
	if (isPrimary && t.params.SubscriberAsPrimary) || (!isPrimary && !t.params.SubscriberAsPrimary) {
		pcTransport = t.subscriber
	}

	return pcTransport
}

func (t *TransportManager) handleConnectionFailed(isShortLived bool) {
	if !t.params.AllowTCPFallback || !isShortLived {
		return
	}
	t.lock.RLock()
	iceConfig := t.iceConfig
	t.lock.RUnlock()

	var nextConfig types.IceConfig
	// irrespective of which one fails, force prefer candidate on both as the other one might
	// fail at a different time and cause another disruption
	switch iceConfig.PreferSub {
	case types.PreferNone:
		t.params.Logger.Infow("restricting transport to TCP on both peer connections")
		nextConfig = types.IceConfig{
			PreferPub: types.PreferTcp,
			PreferSub: types.PreferTcp,
		}

	case types.PreferTcp:
		t.params.Logger.Infow("prefer transport to TLS on both peer connections")
		nextConfig = types.IceConfig{
			PreferPub: types.PreferTls,
			PreferSub: types.PreferTls,
		}

	default:
		return
	}

	t.SetICEConfig(nextConfig)
}

func (t *TransportManager) SetMigrateInfo(previousAnswer *webrtc.SessionDescription, dataChannels []*livekit.DataChannelInfo) {
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

	t.subscriber.SetPreviousAnswer(previousAnswer)
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
