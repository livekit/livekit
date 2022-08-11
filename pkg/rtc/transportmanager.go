package rtc

import (
	"strings"
	"sync"

	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

/* RAJA-TODO
const (
	LossyDataChannel    = "_lossy"
	ReliableDataChannel = "_reliable"

	sdBatchSize       = 20
	rttUpdateInterval = 5 * time.Second

	stateActiveCond           = 3 // reliableDCOpen,lossyDCOpen,PeerConnectionStateConnected
	disconnectCleanupDuration = 15 * time.Second
)
*/

type TransportManagerParams struct {
	Identity                livekit.ParticipantIdentity
	SID                     livekit.ParticipantID
	Config                  *WebRTCConfig
	ProtocolVersion         types.ProtocolVersion
	Telemetry               telemetry.TelemetryService
	CongestionControlConfig config.CongestionControlConfig
	EnabledCodecs           []*livekit.Codec
	Logger                  logger.Logger
	SimTracks               map[uint32]SimulcastTrackInfo
	ClientConf              *livekit.ClientConfiguration
}

type TransportManager struct {
	params TransportManagerParams

	publisher           *PCTransport
	subscriber          *PCTransport
	subscriberAsPrimary bool

	// reliable and unreliable data channels
	reliableDC    *webrtc.DataChannel
	reliableDCSub *webrtc.DataChannel
	lossyDC       *webrtc.DataChannel
	lossyDCSub    *webrtc.DataChannel

	lock sync.RWMutex

	pendingOffer        *webrtc.SessionDescription
	pendingDataChannels []*livekit.DataChannelInfo

	activeCounter atomic.Int32
	iceConfig     types.IceConfig
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
	})
	if err != nil {
		return nil, err
	}
	t.publisher = publisher
	t.publisher.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		/* RAJA-TODO
		if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_PUBLISHER)
		*/
	})
	// RAJA_TODO t.publisher.OnRemoteDescripitonSettled(t.createPublsiherAnswerAndSend)

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
	})
	if err != nil {
		return nil, err
	}
	t.subscriber = subscriber
	// RAJA-TODO t.subscriber.OnNegotiationFailed(p.handleNegotiationFailed)
	t.subscriber.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		/* RAJA-TODO
		if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED || p.MigrateState() == types.MigrateStateInit {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_SUBSCRIBER)
		*/
	})

	/* RAJA-TODO
	primaryPC := p.publisher.pc
	secondaryPC := p.subscriber.pc
	// primary connection does not change, canSubscribe can change if permission was updated
	// after the participant has joined
	p.subscriberAsPrimary = p.ProtocolVersion().SubscriberAsPrimary() && p.CanSubscribe()
	if p.SubscriberAsPrimary() {
		primaryPC = p.subscriber.pc
		secondaryPC = p.publisher.pc
		if !params.Migration {
			if err := p.createDataChannelForSubscriberAsPrimary(nil); err != nil {
				return nil, err
			}
		}
	} else {
		p.activeCounter.Add(2)
	}

	primaryPC.OnConnectionStateChange(p.handlePrimaryStateChange)
	secondaryPC.OnConnectionStateChange(p.handleSecondaryStateChange)

	p.publisher.pc.OnTrack(p.onMediaTrack)
	p.publisher.pc.OnDataChannel(p.onDataChannel)

	p.subscriber.OnOffer(p.onOffer)
	p.subscriber.OnStreamStateChange(p.onStreamStateChange)
	*/

	return t, nil
}

func (t *TransportManager) OnPublisherICECandidate(f func(c *webrtc.ICECandidate)) {
	t.publisher.OnICECandidate(f)
}

func (t *TransportManager) OnSubscriberICECandidate(f func(c *webrtc.ICECandidate)) {
	t.subscriber.OnICECandidate(f)
}

func (t *TransportManager) OnSubscriberNegotiationFailed(f func()) {
	t.subscriber.OnNegotiationFailed(f)
}

/*
func (p *ParticipantImpl) createDataChannelForSubscriberAsPrimary(pendingDataChannels []*livekit.DataChannelInfo) error {
	primaryPC := p.subscriber.pc
	ordered := true
	var (
		reliableID, lossyID       uint16
		reliableIDPtr, lossyIDPtr *uint16
	)
	// for old version migration clients, they don't send subscriber data channel info
	// so we need to create data channels with default ID and don't negotiate as client already has
	// data channels with default ID.
	// for new version migration clients, we create data channels with new ID and negotiate with client

	for _, dc := range pendingDataChannels {
		if dc.Target == livekit.SignalTarget_SUBSCRIBER {
			if dc.Label == ReliableDataChannel {
				// pion use step 2 for auto generated ID, so we need to add 4 to avoid conflict
				reliableID = uint16(dc.Id) + 4
				reliableIDPtr = &reliableID
			} else if dc.Label == LossyDataChannel {
				lossyID = uint16(dc.Id) + 4
				lossyIDPtr = &lossyID
			}
		}
	}

	p.lock.Lock()
	var err error
	negotiated := p.params.Migration && reliableIDPtr == nil
	p.reliableDCSub, err = primaryPC.CreateDataChannel(ReliableDataChannel, &webrtc.DataChannelInit{
		Ordered:    &ordered,
		ID:         reliableIDPtr,
		Negotiated: &negotiated,
	})
	if err != nil {
		p.lock.Unlock()
		return err
	}
	p.reliableDCSub.OnOpen(p.incActiveCounter)

	retransmits := uint16(0)
	negotiated = p.params.Migration && lossyIDPtr == nil
	p.lossyDCSub, err = primaryPC.CreateDataChannel(LossyDataChannel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &retransmits,
		ID:             lossyIDPtr,
		Negotiated:     &negotiated,
	})
	if err != nil {
		p.lock.Unlock()
		return err
	}
	p.lossyDCSub.OnOpen(p.incActiveCounter)
	p.lock.Unlock()

	return nil
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(offer webrtc.SessionDescription) error {
	p.lock.Lock()
	if p.MigrateState() == types.MigrateStateInit {
		p.pendingOffer = &offer
		p.lock.Unlock()
		return nil
	}
	p.lock.Unlock()

	// filter before setting remote description so that pion does not see filtered remote candidates
	if p.iceConfig.PreferPubTcp {
		p.publisher.Logger().Infow("remote offer (unfiltered)", "sdp", offer.SDP)
	}
	modifiedOffer := p.publisher.FilterCandidates(offer)
	if p.iceConfig.PreferPubTcp {
		p.publisher.Logger().Infow("remote offer (filtered)", "sdp", modifiedOffer.SDP)
	}
	if err := p.publisher.SetRemoteDescription(modifiedOffer); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "remote_description").Add(1)
		return err
	}

	return nil
}

func (p *ParticipantImpl) createPublsiherAnswerAndSend() error {
	p.lock.RLock()
	onParticipantUpdate := p.onParticipantUpdate
	p.lock.RUnlock()
	p.configureReceiverDTX()

	answer, err := p.publisher.pc.CreateAnswer(nil)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "create").Add(1)
		return errors.Wrap(err, "could not create answer")
	}

	if p.iceConfig.PreferPubTcp {
		p.publisher.Logger().Infow("local answer (unfiltered)", "sdp", answer.SDP)
	}
	if err = p.publisher.pc.SetLocalDescription(answer); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "local_description").Add(1)
		return errors.Wrap(err, "could not set local description")
	}

	//
	// Filter after setting local description as pion expects the answer
	// to match between CreateAnswer and SetLocalDescription.
	// Filtered answer is sent to remote so that remote does not
	// see filtered candidates.
	//
	answer = p.publisher.FilterCandidates(answer)
	if p.iceConfig.PreferPubTcp {
		p.publisher.Logger().Infow("local answer (filtered)", "sdp", answer.SDP)
	}
	err = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "write_message").Add(1)
		return err
	}

	if p.isPublisher.Load() != p.CanPublish() {
		p.isPublisher.Store(p.CanPublish())
		// trigger update as well if participant is already fully connected
		if p.State() == livekit.ParticipantInfo_ACTIVE && onParticipantUpdate != nil {
			onParticipantUpdate(p)
		}
	}

	prometheus.ServiceOperationCounter.WithLabelValues("answer", "success", "").Add(1)

	if p.MigrateState() == types.MigrateStateSync {
		go p.handleMigrateMutedTrack()
	}

	return nil
}

// RAJA-TODO
func (p *ParticipantImpl) SetMigrateInfo(previousAnswer *webrtc.SessionDescription, mediaTracks []*livekit.TrackPublishedResponse, dataChannels []*livekit.DataChannelInfo) {
	p.pendingTracksLock.Lock()
	for _, t := range mediaTracks {
		p.pendingTracks[t.GetCid()] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{t.GetTrack()}, migrated: true}
	}
	p.pendingDataChannels = dataChannels
	p.pendingTracksLock.Unlock()

	if p.SubscriberAsPrimary() {
		if err := p.createDataChannelForSubscriberAsPrimary(dataChannels); err != nil {
			p.params.Logger.Errorw("create data channel failed", err)
		}
	}

	p.subscriber.SetPreviousAnswer(previousAnswer)
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (p *ParticipantImpl) HandleAnswer(answer webrtc.SessionDescription) error {
	if answer.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}

	// filter before setting remote description so that pion does not see filtered remote candidates
	if p.iceConfig.PreferSubTcp {
		p.subscriber.Logger().Infow("remote answer (unfiltered)", "sdp", answer.SDP)
	}
	modifiedAnswer := p.subscriber.FilterCandidates(answer)
	if p.iceConfig.PreferSubTcp {
		p.subscriber.Logger().Infow("remote answer (filtered)", "sdp", modifiedAnswer.SDP)
	}
	if err := p.subscriber.SetRemoteDescription(modifiedAnswer); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}

	return nil
}

// AddICECandidate adds candidates for remote peer
func (p *ParticipantImpl) AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) error {
	var filterOut bool
	var pcTransport *PCTransport
	p.lock.RLock()
	if target == livekit.SignalTarget_SUBSCRIBER {
		if p.iceConfig.PreferSubTcp && !strings.Contains(candidate.Candidate, "tcp") {
			filterOut = true
			pcTransport = p.subscriber
		}
	} else if target == livekit.SignalTarget_PUBLISHER {
		if p.iceConfig.PreferPubTcp && !strings.Contains(candidate.Candidate, "tcp") {
			filterOut = true
			pcTransport = p.publisher
		}
	}
	p.lock.RUnlock()
	if filterOut {
		pcTransport.Logger().Infow("filtering out remote candidate", "candidate", candidate.Candidate)
		return nil
	}

	var err error
	if target == livekit.SignalTarget_PUBLISHER {
		err = p.publisher.AddICECandidate(candidate)
	} else {
		err = p.subscriber.AddICECandidate(candidate)
	}
	return err
}

// Negotiate subscriber SDP with client, if force is true, will cencel pending
// negotiate task and negotiate immediately
func (p *ParticipantImpl) Negotiate(force bool) {
	if p.MigrateState() != types.MigrateStateInit {
		p.subscriber.Negotiate(force)
	}
}

func (p *ParticipantImpl) AddNegotiationPending(publisherID livekit.ParticipantID) {
	p.subscriber.AddNegotiationPending(publisherID)
}

func (p *ParticipantImpl) IsNegotiationPending(publisherID livekit.ParticipantID) bool {
	return p.subscriber.IsNegotiationPending(publisherID)
}

// RAJA-TODO
func (p *ParticipantImpl) SetMigrateState(s types.MigrateState) {
	p.lock.Lock()
	preState := p.MigrateState()
	if preState == types.MigrateStateComplete || preState == s {
		p.lock.Unlock()
		return
	}

	p.params.Logger.Debugw("SetMigrateState", "state", s)
	var pendingOffer *webrtc.SessionDescription
	p.migrateState.Store(s)
	if s == types.MigrateStateSync {
		pendingOffer = p.pendingOffer
		p.pendingOffer = nil
	}
	p.lock.Unlock()

	if s == types.MigrateStateComplete {
		p.pendingTracksLock.Lock()
		pendingDataChannels := p.pendingDataChannels
		p.pendingDataChannels = nil
		p.pendingTracksLock.Unlock()

		p.handlePendingPublisherDataChannels(pendingDataChannels)
	}

	if pendingOffer != nil {
		err := p.HandleOffer(*pendingOffer)
		if err != nil {
			p.GetLogger().Errorw("could not handle offer", err)
		}
	}
}

// ICERestart restarts subscriber ICE connections
func (p *ParticipantImpl) ICERestart(iceConfig *types.IceConfig) error {
	if iceConfig != nil {
		p.SetICEConfig(*iceConfig)
	}

	if p.subscriber.pc.RemoteDescription() == nil {
		// not connected, skip
		return nil
	}

	for _, t := range p.GetPublishedTracks() {
		t.(types.LocalMediaTrack).Restart()
	}

	return p.subscriber.CreateAndSendOffer(&webrtc.OfferOptions{
		ICERestart: true,
	})
}

func (p *ParticipantImpl) OnICEConfigChanged(f func(participant types.LocalParticipant, iceConfig types.IceConfig)) {
	p.lock.Lock()
	p.onICEConfigChanged = f
	p.lock.Unlock()
}

func (p *ParticipantImpl) SetICEConfig(iceConfig types.IceConfig) {
	p.params.Logger.Infow("setting ICE config", "iceConfig", iceConfig)
	p.lock.Lock()
	p.iceConfig = iceConfig
	if iceConfig.PreferPubTcp {
		p.publisher.SetPreferTCP(true)
	}

	if iceConfig.PreferSubTcp {
		p.subscriber.SetPreferTCP(true)
	}

	onICEConfigChanged := p.onICEConfigChanged
	p.lock.Unlock()

	if onICEConfigChanged != nil {
		onICEConfigChanged(p, iceConfig)
	}
}
*/

func (t *TransportManager) SubscriberAsPrimary() bool {
	return t.subscriberAsPrimary
}

/*
func (p *ParticipantImpl) SubscriberPC() *webrtc.PeerConnection {
	return p.subscriber.pc
}

// when the server has an offer for participant
func (p *ParticipantImpl) onOffer(offer webrtc.SessionDescription) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		// skip when disconnected
		return
	}

	err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "write_message").Add(1)
	} else {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "success", "").Add(1)
	}
}

// RAJA-TODO
// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return
	}

	if !p.CanPublish() {
		p.params.Logger.Warnw("no permission to publish mediaTrack", nil)
		return
	}

	publishedTrack, isNewTrack := p.mediaTrackReceived(track, rtpReceiver)

	if publishedTrack != nil {
		p.params.Logger.Infow("mediaTrack published",
			"kind", track.Kind().String(),
			"trackID", publishedTrack.ID(),
			"rid", track.RID(),
			"SSRC", track.SSRC())
	} else {
		p.params.Logger.Warnw("webrtc Track published but can't find MediaTrack", nil,
			"kind", track.Kind().String(),
			"webrtcTrackID", track.ID(),
			"rid", track.RID(),
			"SSRC", track.SSRC())
	}

	if !isNewTrack && publishedTrack != nil && !publishedTrack.HasPendingCodec() && p.IsReady() {
		p.lock.RLock()
		onTrackUpdated := p.onTrackUpdated
		p.lock.RUnlock()
		if onTrackUpdated != nil {
			onTrackUpdated(p, publishedTrack)
		}
	}
}

func (p *ParticipantImpl) onDataChannel(dc *webrtc.DataChannel) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return
	}
	switch dc.Label() {
	case ReliableDataChannel:
		p.lock.Lock()
		p.reliableDC = dc
		p.lock.Unlock()
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if p.CanPublishData() {
				p.handleDataMessage(livekit.DataPacket_RELIABLE, msg.Data)
			}
		})
	case LossyDataChannel:
		p.lock.Lock()
		p.lossyDC = dc
		p.lock.Unlock()
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if p.CanPublishData() {
				p.handleDataMessage(livekit.DataPacket_LOSSY, msg.Data)
			}
		})
	default:
		p.params.Logger.Warnw("unsupported datachannel added", nil, "label", dc.Label())
	}
}
*/

func (t *TransportManager) getTransport(isPrimary bool) *PCTransport {
	pcTransport := t.publisher
	subscriberAsPrimary := t.SubscriberAsPrimary()
	if (isPrimary && subscriberAsPrimary) || (!isPrimary && !subscriberAsPrimary) {
		pcTransport = t.subscriber
	}

	return pcTransport
}

/*
RAJA-TODO - handle connection failed callback and TCP fallback
*/

/*
func (p *ParticipantImpl) handlePrimaryStateChange(state webrtc.PeerConnectionState) {
	if state == webrtc.PeerConnectionStateConnected {
		if !p.firstConnected.Swap(true) {
			p.setDowntracksConnected()
		}
		prometheus.ServiceOperationCounter.WithLabelValues("ice_connection", "success", "").Add(1)
		if !p.hasPendingMigratedTrack() && p.MigrateState() == types.MigrateStateSync {
			p.SetMigrateState(types.MigrateStateComplete)
		}
		p.incActiveCounter()
	} else if state == webrtc.PeerConnectionStateFailed {
		p.handleConnectionFailed(true)

		// clients support resuming of connections when websocket becomes disconnected
		p.closeSignalConnection()

		// detect when participant has actually left.
		go func() {
			p.lock.Lock()
			if p.disconnectTimer != nil {
				p.disconnectTimer.Stop()
				p.disconnectTimer = nil
			}
			p.disconnectTimer = time.AfterFunc(disconnectCleanupDuration, func() {
				p.lock.Lock()
				p.disconnectTimer.Stop()
				p.disconnectTimer = nil
				p.lock.Unlock()

				if p.isClosed.Load() || p.State() == livekit.ParticipantInfo_DISCONNECTED {
					return
				}
				primaryPC := p.publisher.pc
				if p.SubscriberAsPrimary() {
					primaryPC = p.subscriber.pc
				}
				if primaryPC.ConnectionState() != webrtc.PeerConnectionStateConnected {
					p.params.Logger.Infow("closing disconnected participant")
					_ = p.Close(true, types.ParticipantCloseReasonPeerConnectionDisconnected)
				}
			})
			p.lock.Unlock()
		}()

	}
}

// for the secondary peer connection, we still need to handle when they become disconnected
// instead of allowing them to silently fail.
func (p *ParticipantImpl) handleSecondaryStateChange(state webrtc.PeerConnectionState) {
	if state == webrtc.PeerConnectionStateFailed {
		p.handleConnectionFailed(false)

		// clients support resuming of connections when websocket becomes disconnected
		p.closeSignalConnection()
	}
}

func (p *ParticipantImpl) handlePendingPublisherDataChannels(pendingDataChannels []*livekit.DataChannelInfo) {
	ordered := true
	negotiated := true

	p.lock.RLock()
	lossyDC := p.lossyDC
	reliableDC := p.reliableDC
	p.lock.RUnlock()

	for _, ci := range pendingDataChannels {
		var (
			dc  *webrtc.DataChannel
			err error
		)
		if ci.Target == livekit.SignalTarget_SUBSCRIBER {
			continue
		}
		if ci.Label == LossyDataChannel && lossyDC == nil {
			retransmits := uint16(0)
			id := uint16(ci.GetId())
			dc, err = p.publisher.pc.CreateDataChannel(LossyDataChannel, &webrtc.DataChannelInit{
				Ordered:        &ordered,
				MaxRetransmits: &retransmits,
				Negotiated:     &negotiated,
				ID:             &id,
			})
		} else if ci.Label == ReliableDataChannel && reliableDC == nil {
			id := uint16(ci.GetId())
			dc, err = p.publisher.pc.CreateDataChannel(ReliableDataChannel, &webrtc.DataChannelInit{
				Ordered:    &ordered,
				Negotiated: &negotiated,
				ID:         &id,
			})
		}
		if err != nil {
			p.params.Logger.Errorw("create migrated data channel failed", err, "label", ci.Label)
		} else if dc != nil {
			p.params.Logger.Debugw("create migrated data channel", "label", dc.Label(), "id", dc.ID())
			p.onDataChannel(dc)
		}
	}
}

func (p *ParticipantImpl) incActiveCounter() {
	if p.activeCounter.Inc() == stateActiveCond {
		p.updateState(livekit.ParticipantInfo_ACTIVE)
	}
}

func (p *ParticipantImpl) handleNegotiationFailed() {
	p.params.Logger.Infow("negotiation failed, starting full reconnect")
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: &livekit.LeaveRequest{
				CanReconnect: true,
				Reason:       types.ParticipantCloseReasonNegotiateFailed.ToDisconnectReason(),
			},
		},
	})
	p.closeSignalConnection()
}
*/
