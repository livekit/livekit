package rtc

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

const (
	LossyDataChannel    = "_lossy"
	ReliableDataChannel = "_reliable"

	sdBatchSize       = 20
	rttUpdateInterval = 5 * time.Second

	stateActiveCond           = 3 // reliableDCOpen,lossyDCOpen,PeerConnectionStateConnected
	disconnectCleanupDuration = 15 * time.Second
)

type pendingTrackInfo struct {
	*livekit.TrackInfo
	migrated bool
}

type downTrackState struct {
	transceiver *webrtc.RTPTransceiver
	forwarder   sfu.ForwarderState
}

type ParticipantParams struct {
	Identity                livekit.ParticipantIdentity
	Name                    livekit.ParticipantName
	SID                     livekit.ParticipantID
	Config                  *WebRTCConfig
	Sink                    routing.MessageSink
	AudioConfig             config.AudioConfig
	VideoConfig             config.VideoConfig
	ProtocolVersion         types.ProtocolVersion
	Telemetry               telemetry.TelemetryService
	PLIThrottleConfig       config.PLIThrottleConfig
	CongestionControlConfig config.CongestionControlConfig
	EnabledCodecs           []*livekit.Codec
	Logger                  logger.Logger
	SimTracks               map[uint32]SimulcastTrackInfo
	Grants                  *auth.ClaimGrants
	InitialVersion          uint32
	ClientConf              *livekit.ClientConfiguration
	Region                  string
	Migration               bool
	AdaptiveStream          bool
}

type ParticipantImpl struct {
	params              ParticipantParams
	publisher           *PCTransport
	subscriber          *PCTransport
	isClosed            atomic.Bool
	state               atomic.Value // livekit.ParticipantInfo_State
	updateCache         *lru.Cache
	resSink             atomic.Value // routing.MessageSink
	resSinkValid        atomic.Bool
	subscriberAsPrimary bool
	grants              *auth.ClaimGrants
	isPublisher         atomic.Bool

	// reliable and unreliable data channels
	reliableDC    *webrtc.DataChannel
	reliableDCSub *webrtc.DataChannel
	lossyDC       *webrtc.DataChannel
	lossyDCSub    *webrtc.DataChannel

	// when first connected
	connectedAt time.Time
	// timer that's set when disconnect is detected on primary PC
	disconnectTimer *time.Timer

	rtcpCh chan []rtcp.Packet

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// client intended to publish, yet to be reconciled
	pendingTracksLock sync.RWMutex
	pendingTracks     map[string]*pendingTrackInfo

	*UpTrackManager

	// tracks the current participant is subscribed to
	subscribedTracks map[livekit.TrackID]types.SubscribedTrack
	// track settings of tracks the current participant is subscribed to
	subscribedTracksSettings map[livekit.TrackID]*livekit.UpdateTrackSettings
	// keeps track of disallowed tracks
	disallowedSubscriptions map[livekit.TrackID]livekit.ParticipantID // trackID -> publisherID
	// keep track of other publishers ids that we are subscribed to
	subscribedTo      map[livekit.ParticipantID]struct{}
	unpublishedTracks []*livekit.TrackInfo

	rttUpdatedAt time.Time
	lastRTT      uint32

	lock       sync.RWMutex
	once       sync.Once
	updateLock sync.Mutex
	version    atomic.Uint32

	// callbacks & handlers
	onTrackPublished    func(types.LocalParticipant, types.MediaTrack)
	onTrackUpdated      func(types.LocalParticipant, types.MediaTrack)
	onStateChange       func(p types.LocalParticipant, oldState livekit.ParticipantInfo_State)
	onParticipantUpdate func(types.LocalParticipant)
	onDataPacket        func(types.LocalParticipant, *livekit.DataPacket)
	onSubscribedTo      func(types.LocalParticipant, livekit.ParticipantID)

	migrateState        atomic.Value // types.MigrateState
	pendingOffer        *webrtc.SessionDescription
	pendingDataChannels []*livekit.DataChannelInfo
	onClose             func(types.LocalParticipant, map[livekit.TrackID]livekit.ParticipantID)
	onClaimsChanged     func(participant types.LocalParticipant)

	activeCounter  atomic.Int32
	firstConnected atomic.Bool
	iceConfig      types.IceConfig

	cachedDownTracks map[livekit.TrackID]*downTrackState
}

func NewParticipant(params ParticipantParams) (*ParticipantImpl, error) {
	if params.Identity == "" {
		return nil, ErrEmptyIdentity
	}
	if params.SID == "" {
		return nil, ErrEmptyParticipantID
	}
	if params.Grants == nil || params.Grants.Video == nil {
		return nil, ErrMissingGrants
	}
	p := &ParticipantImpl{
		params:                   params,
		rtcpCh:                   make(chan []rtcp.Packet, 100),
		pendingTracks:            make(map[string]*pendingTrackInfo),
		subscribedTracks:         make(map[livekit.TrackID]types.SubscribedTrack),
		subscribedTracksSettings: make(map[livekit.TrackID]*livekit.UpdateTrackSettings),
		disallowedSubscriptions:  make(map[livekit.TrackID]livekit.ParticipantID),
		subscribedTo:             make(map[livekit.ParticipantID]struct{}),
		connectedAt:              time.Now(),
		rttUpdatedAt:             time.Now(),
		cachedDownTracks:         make(map[livekit.TrackID]*downTrackState),
	}
	p.version.Store(params.InitialVersion)
	p.migrateState.Store(types.MigrateStateInit)
	p.state.Store(livekit.ParticipantInfo_JOINING)
	p.grants = params.Grants
	p.SetResponseSink(params.Sink)

	var err error
	// keep last participants and when updates were sent
	if p.updateCache, err = lru.New(32); err != nil {
		return nil, err
	}

	enabledCodecs := make([]*livekit.Codec, 0, len(p.params.EnabledCodecs))
	for _, c := range p.params.EnabledCodecs {
		var disabled bool
		for _, disableCodec := range p.params.ClientConf.GetDisabledCodecs().GetCodecs() {
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

	p.publisher, err = NewPCTransport(TransportParams{
		ParticipantID:           p.params.SID,
		ParticipantIdentity:     p.params.Identity,
		ProtocolVersion:         p.ProtocolVersion(),
		Target:                  livekit.SignalTarget_PUBLISHER,
		Config:                  params.Config,
		CongestionControlConfig: params.CongestionControlConfig,
		Telemetry:               p.params.Telemetry,
		EnabledCodecs:           enabledCodecs,
		Logger:                  LoggerWithPCTarget(params.Logger, livekit.SignalTarget_PUBLISHER),
		SimTracks:               params.SimTracks,
	})
	if err != nil {
		return nil, err
	}
	p.subscriber, err = NewPCTransport(TransportParams{
		ParticipantID:           p.params.SID,
		ParticipantIdentity:     p.params.Identity,
		ProtocolVersion:         p.ProtocolVersion(),
		Target:                  livekit.SignalTarget_SUBSCRIBER,
		Config:                  params.Config,
		CongestionControlConfig: params.CongestionControlConfig,
		Telemetry:               p.params.Telemetry,
		EnabledCodecs:           enabledCodecs,
		Logger:                  LoggerWithPCTarget(params.Logger, livekit.SignalTarget_SUBSCRIBER),
	})
	if err != nil {
		return nil, err
	}

	p.subscriber.OnNegotiationFailed(p.handleNegotiationFailed)

	p.publisher.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_PUBLISHER)
	})
	p.subscriber.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED || p.MigrateState() == types.MigrateStateInit {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_SUBSCRIBER)
	})

	primaryPC := p.publisher.pc
	secondaryPC := p.subscriber.pc
	// primary connection does not change, canSubscribe can change if permission was updated
	// after the participant has joined
	p.subscriberAsPrimary = p.ProtocolVersion().SubscriberAsPrimary() && p.CanSubscribe()
	if p.SubscriberAsPrimary() {
		primaryPC = p.subscriber.pc
		secondaryPC = p.publisher.pc
		if !params.Migration {
			if err := p.createDataChannelForSubscriberAsPrimary(); err != nil {
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

	p.setupUpTrackManager()

	return p, nil
}

func (p *ParticipantImpl) createDataChannelForSubscriberAsPrimary() error {
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

	for _, dc := range p.pendingDataChannels {
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

	var err error
	negotiated := p.params.Migration && reliableIDPtr == nil
	p.reliableDCSub, err = primaryPC.CreateDataChannel(ReliableDataChannel, &webrtc.DataChannelInit{
		Ordered:    &ordered,
		ID:         reliableIDPtr,
		Negotiated: &negotiated,
	})
	if err != nil {
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
		return err
	}
	p.lossyDCSub.OnOpen(p.incActiveCounter)
	return nil
}

func (p *ParticipantImpl) GetLogger() logger.Logger {
	return p.params.Logger
}

func (p *ParticipantImpl) GetAdaptiveStream() bool {
	return p.params.AdaptiveStream
}

func (p *ParticipantImpl) ID() livekit.ParticipantID {
	return p.params.SID
}

func (p *ParticipantImpl) Identity() livekit.ParticipantIdentity {
	return p.params.Identity
}

func (p *ParticipantImpl) State() livekit.ParticipantInfo_State {
	return p.state.Load().(livekit.ParticipantInfo_State)
}

func (p *ParticipantImpl) ProtocolVersion() types.ProtocolVersion {
	return p.params.ProtocolVersion
}

func (p *ParticipantImpl) IsReady() bool {
	state := p.State()
	return state == livekit.ParticipantInfo_JOINED || state == livekit.ParticipantInfo_ACTIVE
}

func (p *ParticipantImpl) ConnectedAt() time.Time {
	return p.connectedAt
}

// SetMetadata attaches metadata to the participant
func (p *ParticipantImpl) SetMetadata(metadata string) {
	p.lock.Lock()
	changed := p.grants.Metadata != metadata
	p.grants.Metadata = metadata
	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged
	p.lock.Unlock()

	if !changed {
		return
	}

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
}

func (p *ParticipantImpl) ClaimGrants() *auth.ClaimGrants {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Clone()
}

func (p *ParticipantImpl) SetPermission(permission *livekit.ParticipantPermission) bool {
	if permission == nil {
		return false
	}
	p.lock.Lock()
	video := p.grants.Video
	hasChanged := video.GetCanSubscribe() != permission.CanSubscribe ||
		video.GetCanPublish() != permission.CanPublish ||
		video.GetCanPublishData() != permission.CanPublishData ||
		video.Hidden != permission.Hidden ||
		video.Recorder != permission.Recorder

	if !hasChanged {
		p.lock.Unlock()
		return false
	}

	video.SetCanSubscribe(permission.CanSubscribe)
	video.SetCanPublish(permission.CanPublish)
	video.SetCanPublishData(permission.CanPublishData)
	video.Hidden = permission.Hidden
	video.Recorder = permission.Recorder

	canPublish := video.GetCanPublish()
	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged
	p.lock.Unlock()

	// publish permission has been revoked then remove all published tracks
	if !canPublish {
		for _, track := range p.GetPublishedTracks() {
			p.RemovePublishedTrack(track, false)
			if p.ProtocolVersion().SupportsUnpublish() {
				p.sendTrackUnpublished(track.ID())
			} else {
				// for older clients that don't support unpublish, mute to avoid them sending data
				p.sendTrackMuted(track.ID(), true)
			}
		}
	}
	// update isPublisher attribute
	p.isPublisher.Store(canPublish && p.publisher.IsEstablished())

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
	return true
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	p.lock.RLock()
	info := &livekit.ParticipantInfo{
		Sid:         string(p.params.SID),
		Identity:    string(p.params.Identity),
		Name:        string(p.params.Name),
		State:       p.State(),
		JoinedAt:    p.ConnectedAt().Unix(),
		Version:     p.version.Inc(),
		Permission:  p.grants.Video.ToPermission(),
		Metadata:    p.grants.Metadata,
		Region:      p.params.Region,
		IsPublisher: p.IsPublisher(),
	}
	p.lock.RUnlock()
	info.Tracks = p.UpTrackManager.ToProto()

	return info
}

func (p *ParticipantImpl) SubscriberMediaEngine() *webrtc.MediaEngine {
	return p.subscriber.me
}

// callbacks for clients

func (p *ParticipantImpl) OnTrackPublished(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackPublished = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnStateChange(callback func(p types.LocalParticipant, oldState livekit.ParticipantInfo_State)) {
	p.lock.Lock()
	p.onStateChange = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnTrackUpdated(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackUpdated = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnParticipantUpdate(callback func(types.LocalParticipant)) {
	p.lock.Lock()
	p.onParticipantUpdate = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnDataPacket(callback func(types.LocalParticipant, *livekit.DataPacket)) {
	p.lock.Lock()
	p.onDataPacket = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnSubscribedTo(callback func(types.LocalParticipant, livekit.ParticipantID)) {
	p.lock.Lock()
	p.onSubscribedTo = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnClose(callback func(types.LocalParticipant, map[livekit.TrackID]livekit.ParticipantID)) {
	p.lock.Lock()
	p.onClose = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnClaimsChanged(callback func(types.LocalParticipant)) {
	p.onClaimsChanged = callback
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error) {
	p.lock.Lock()
	if p.MigrateState() == types.MigrateStateInit {
		p.pendingOffer = &sdp
		p.lock.Unlock()
		return
	}
	onParticipantUpdate := p.onParticipantUpdate
	p.lock.Unlock()
	p.params.Logger.Debugw("answering pub offer",
		"state", p.State().String(),
		// "sdp", sdp.SDP,
	)

	if err = p.publisher.SetRemoteDescription(sdp); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "remote_description").Add(1)
		return
	}

	p.configureReceiverDTX()

	answer, err = p.publisher.pc.CreateAnswer(nil)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "create").Add(1)
		err = errors.Wrap(err, "could not create answer")
		return
	}

	if err = p.publisher.pc.SetLocalDescription(answer); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "local_description").Add(1)
		err = errors.Wrap(err, "could not set local description")
		return
	}

	p.params.Logger.Debugw("sending answer to client")

	err = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "write_message").Add(1)
		return
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

	return
}

func (p *ParticipantImpl) handleMigrateMutedTrack() {
	// muted track won't send rtp packet, so we add mediatrack manually
	var addedTrack []*MediaTrack
	p.pendingTracksLock.Lock()
	for cid, t := range p.pendingTracks {
		if t.migrated && t.Muted && t.Type == livekit.TrackType_VIDEO {
			addedTrack = append(addedTrack, p.addMigrateMutedTrack(cid, t.TrackInfo))
		}
	}
	p.pendingTracksLock.Unlock()

	for _, t := range addedTrack {
		if t != nil {
			p.handleTrackPublished(t)
		}
	}
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (p *ParticipantImpl) AddTrack(req *livekit.AddTrackRequest) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.grants.Video.GetCanPublish() {
		p.params.Logger.Warnw("no permission to publish track", nil)
		return
	}

	ti := p.addPendingTrackLocked(req)
	if ti == nil {
		return
	}

	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   req.Cid,
				Track: ti,
			},
		},
	})
}

func (p *ParticipantImpl) SetMigrateInfo(previousAnswer *webrtc.SessionDescription, mediaTracks []*livekit.TrackPublishedResponse, dataChannels []*livekit.DataChannelInfo) {
	p.pendingTracksLock.Lock()
	for _, t := range mediaTracks {
		pendingInfo := &pendingTrackInfo{TrackInfo: t.GetTrack(), migrated: true}
		p.pendingTracks[t.GetCid()] = pendingInfo
	}
	p.pendingDataChannels = dataChannels

	if p.SubscriberAsPrimary() {
		if err := p.createDataChannelForSubscriberAsPrimary(); err != nil {
			p.params.Logger.Errorw("create data channel failed", err)
		}
	}
	p.pendingTracksLock.Unlock()
	p.subscriber.SetPreviousAnswer(previousAnswer)
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (p *ParticipantImpl) HandleAnswer(sdp webrtc.SessionDescription) error {
	if sdp.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}
	p.params.Logger.Debugw("setting subPC answer")

	if err := p.subscriber.SetRemoteDescription(sdp); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}

	return nil
}

// AddICECandidate adds candidates for remote peer
func (p *ParticipantImpl) AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) error {
	var filterOut bool
	p.lock.RLock()
	if target == livekit.SignalTarget_SUBSCRIBER {
		if p.iceConfig.PreferSubTcp && !strings.Contains(candidate.Candidate, "tcp") {
			filterOut = true
		}
	} else if target == livekit.SignalTarget_PUBLISHER {
		if p.iceConfig.PreferPubTcp && !strings.Contains(candidate.Candidate, "tcp") {
			filterOut = true
		}
	}
	p.lock.RUnlock()
	if filterOut {
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

func (p *ParticipantImpl) Start() {
	p.once.Do(func() {
		p.UpTrackManager.Start()
		go p.rtcpSendWorker()
		go p.downTracksRTCPWorker()
	})
}

func (p *ParticipantImpl) Close(sendLeave bool, reason types.ParticipantCloseReason) error {
	if p.isClosed.Swap(true) {
		// already closed
		return nil
	}

	p.params.Logger.Infow("closing participant", "sendLeave", sendLeave, "reason", reason)
	// send leave message
	if sendLeave {
		_ = p.writeMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{
				Leave: &livekit.LeaveRequest{
					Reason: reason.ToDisconnectReason(),
				},
			},
		})
	}

	p.UpTrackManager.Close(!sendLeave)

	p.pendingTracksLock.Lock()
	p.pendingTracks = make(map[string]*pendingTrackInfo)
	p.pendingTracksLock.Unlock()

	p.lock.Lock()
	disallowedSubscriptions := make(map[livekit.TrackID]livekit.ParticipantID)
	for trackID, publisherID := range p.disallowedSubscriptions {
		disallowedSubscriptions[trackID] = publisherID
	}

	// remove all down tracks
	var downTracksToClose []*sfu.DownTrack
	for _, st := range p.subscribedTracks {
		downTracksToClose = append(downTracksToClose, st.DownTrack())
	}
	p.lock.Unlock()

	for _, dt := range downTracksToClose {
		dt.Close()
	}

	p.updateState(livekit.ParticipantInfo_DISCONNECTED)

	// ensure this is synchronized
	p.closeSignalConnection()
	p.lock.RLock()
	onClose := p.onClose
	p.lock.RUnlock()
	if onClose != nil {
		onClose(p, disallowedSubscriptions)
	}

	// Close peer connections without blocking participant close. If peer connections are gathering candidates
	// Close will block.
	go func() {
		p.publisher.Close()
		p.subscriber.Close()
	}()
	return nil
}

// Negotiate subscriber SDP with client, if force is true, will cencel pending
// negotiate task and negotiate immediately
func (p *ParticipantImpl) Negotiate(force bool) {
	if p.MigrateState() != types.MigrateStateInit {
		p.subscriber.Negotiate(force)
	}
}

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
		p.handlePendingPublisherDataChannels()
	}
	if pendingOffer != nil {
		_, err := p.HandleOffer(*pendingOffer)
		if err != nil {
			p.GetLogger().Errorw("could not handle offer", err)
		}
	}
}

func (p *ParticipantImpl) MigrateState() types.MigrateState {
	return p.migrateState.Load().(types.MigrateState)
}

// ICERestart restarts subscriber ICE connections
func (p *ParticipantImpl) ICERestart(iceConfig *types.IceConfig) error {
	if iceConfig != nil {
		p.lock.Lock()
		p.iceConfig = *iceConfig
		p.lock.Unlock()
	}

	if p.subscriber.pc.RemoteDescription() == nil {
		// not connected, skip
		return nil
	}

	p.UpTrackManager.Restart()

	return p.subscriber.CreateAndSendOffer(&webrtc.OfferOptions{
		ICERestart: true,
	})
}

//
// signal connection methods
//

func (p *ParticipantImpl) GetAudioLevel() (level float64, active bool) {
	level = 0
	for _, pt := range p.GetPublishedTracks() {
		mediaTrack := pt.(types.LocalMediaTrack)
		if mediaTrack.Source() == livekit.TrackSource_MICROPHONE {
			tl, ta := mediaTrack.GetAudioLevel()
			if ta {
				active = true
				if tl > level {
					level = tl
				}
			}
		}
	}
	return
}

func (p *ParticipantImpl) GetConnectionQuality() *livekit.ConnectionQualityInfo {
	publisherScores := p.getPublisherConnectionQuality()

	numTracks := len(publisherScores)
	totalScore := float32(0.0)
	for _, score := range publisherScores {
		totalScore += score
	}

	p.lock.RLock()
	subscriberScores := make(map[livekit.TrackID]float32, len(p.subscribedTracks))
	for _, subTrack := range p.subscribedTracks {
		if subTrack.IsMuted() || subTrack.MediaTrack().IsMuted() {
			continue
		}
		score := subTrack.DownTrack().GetConnectionScore()
		subscriberScores[subTrack.ID()] = score
		totalScore += score
		numTracks++
	}
	p.lock.RUnlock()

	avgScore := float32(5.0)
	if numTracks > 0 {
		avgScore = totalScore / float32(numTracks)
	}

	rating := connectionquality.Score2Rating(avgScore)
	if rating != livekit.ConnectionQuality_EXCELLENT {
		p.params.Logger.Infow("connection quality not optimal",
			"totalScore", totalScore,
			"numTracks", numTracks,
			"rating", rating,
			"publisherScores", publisherScores,
			"subscriberScores", subscriberScores,
		)
	}

	return &livekit.ConnectionQualityInfo{
		ParticipantSid: string(p.ID()),
		Quality:        rating,
		Score:          avgScore,
	}
}

func (p *ParticipantImpl) GetSubscribedParticipants() []livekit.ParticipantID {
	p.lock.RLock()
	defer p.lock.RUnlock()

	var participantIDs []livekit.ParticipantID
	for pID := range p.subscribedTo {
		participantIDs = append(participantIDs, pID)
	}
	return participantIDs
}

func (p *ParticipantImpl) IsSubscribedTo(participantID livekit.ParticipantID) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	_, ok := p.subscribedTo[participantID]
	return ok
}

func (p *ParticipantImpl) IsPublisher() bool {
	return p.isPublisher.Load()
}

func (p *ParticipantImpl) CanPublish() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.GetCanPublish()
}

func (p *ParticipantImpl) CanSubscribe() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.GetCanSubscribe()
}

func (p *ParticipantImpl) CanPublishData() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.GetCanPublishData()
}

func (p *ParticipantImpl) Hidden() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.Hidden
}

func (p *ParticipantImpl) IsRecorder() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.Recorder
}

func (p *ParticipantImpl) SubscriberAsPrimary() bool {
	return p.subscriberAsPrimary
}

func (p *ParticipantImpl) SubscriberPC() *webrtc.PeerConnection {
	return p.subscriber.pc
}

func (p *ParticipantImpl) UpdateSubscribedTrackSettings(trackID livekit.TrackID, settings *livekit.UpdateTrackSettings) error {
	p.lock.Lock()
	p.subscribedTracksSettings[trackID] = settings

	subTrack := p.subscribedTracks[trackID]
	if subTrack == nil {
		p.lock.Unlock()
		p.params.Logger.Warnw("could not find subscribed track", nil, "trackID", trackID)
		return errors.New("could not find subscribed track")
	}
	p.lock.Unlock()

	subTrack.UpdateSubscriberSettings(settings)
	return nil
}

// AddSubscribedTrack adds a track to the participant's subscribed list
func (p *ParticipantImpl) AddSubscribedTrack(subTrack types.SubscribedTrack) {
	p.params.Logger.Debugw("added subscribedTrack",
		"publisherID", subTrack.PublisherID(),
		"publisherIdentity", subTrack.PublisherIdentity(),
		"trackID", subTrack.ID())
	p.lock.Lock()
	onSubscribedTo := p.onSubscribedTo
	p.subscribedTracks[subTrack.ID()] = subTrack
	settings := p.subscribedTracksSettings[subTrack.ID()]
	p.lock.Unlock()

	subTrack.OnBind(func() {
		if p.firstConnected.Load() {
			subTrack.DownTrack().SetConnected()
		}
		p.subscriber.AddTrack(subTrack)
	})

	if settings != nil {
		subTrack.UpdateSubscriberSettings(settings)
	}

	publisherID := subTrack.PublisherID()
	p.lock.Lock()
	_, isAlreadySubscribed := p.subscribedTo[publisherID]
	p.subscribedTo[publisherID] = struct{}{}
	p.lock.Unlock()
	if !isAlreadySubscribed && onSubscribedTo != nil {
		onSubscribedTo(p, publisherID)
	}
}

// RemoveSubscribedTrack removes a track to the participant's subscribed list
func (p *ParticipantImpl) RemoveSubscribedTrack(subTrack types.SubscribedTrack) {
	p.params.Logger.Debugw("removed subscribedTrack",
		"publisherID", subTrack.PublisherID(),
		"publisherIdentity", subTrack.PublisherIdentity(),
		"trackID", subTrack.ID(), "kind", subTrack.DownTrack().Kind())

	p.subscriber.RemoveTrack(subTrack)

	p.lock.Lock()
	delete(p.subscribedTracks, subTrack.ID())
	// remove from subscribed map
	numRemaining := 0
	for _, st := range p.subscribedTracks {
		if st.PublisherID() == subTrack.PublisherID() {
			numRemaining++
		}
	}

	//
	// NOTE
	// subscribedTrackSettings should not be deleted on removal as it is needed if corresponding publisher migrated
	// LK-TODO: find a way to clean these up
	//

	if numRemaining == 0 {
		delete(p.subscribedTo, subTrack.PublisherID())
	}
	p.lock.Unlock()

	if numRemaining == 0 {
		//
		// When a participant leaves OR
		// this participant unsubscribes from all tracks of another participant,
		// have to send speaker update indicating that the participant speaker is no long active
		// so that clients can clean up their speaker state for the leaving/unsubscribed participant
		//
		if p.ProtocolVersion().SupportsSpeakerChanged() {
			_ = p.writeMessage(&livekit.SignalResponse{
				Message: &livekit.SignalResponse_SpeakersChanged{
					SpeakersChanged: &livekit.SpeakersChanged{
						Speakers: []*livekit.SpeakerInfo{
							{
								Sid:    string(subTrack.PublisherID()),
								Level:  0,
								Active: false,
							},
						},
					},
				},
			})
		}
	}
}

func (p *ParticipantImpl) SubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool) {
	p.lock.Lock()
	if allowed {
		delete(p.disallowedSubscriptions, trackID)
	} else {
		p.disallowedSubscriptions[trackID] = publisherID
	}
	p.lock.Unlock()

	p.params.Logger.Debugw("sending subscription permission update", "publisherID", publisherID, "trackID", trackID, "allowed", allowed)
	err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscriptionPermissionUpdate{
			SubscriptionPermissionUpdate: &livekit.SubscriptionPermissionUpdate{
				ParticipantSid: string(publisherID),
				TrackSid:       string(trackID),
				Allowed:        allowed,
			},
		},
	})
	if err != nil {
		p.params.Logger.Errorw("could not send subscription permission update", err)
	}
}

func (p *ParticipantImpl) UpdateRTT(rtt uint32) {
	now := time.Now()
	p.lock.Lock()
	if now.Sub(p.rttUpdatedAt) < rttUpdateInterval || p.lastRTT == rtt {
		p.lock.Unlock()
		return
	}
	p.rttUpdatedAt = now
	p.lastRTT = rtt
	p.lock.Unlock()

	for _, pt := range p.GetPublishedTracks() {
		pt.(types.LocalMediaTrack).SetRTT(rtt)
	}
}

func (p *ParticipantImpl) setupUpTrackManager() {
	p.UpTrackManager = NewUpTrackManager(UpTrackManagerParams{
		SID:    p.params.SID,
		Logger: p.params.Logger,
	})

	p.UpTrackManager.OnPublishedTrackUpdated(func(track types.MediaTrack, onlyIfReady bool) {
		if onlyIfReady && !p.IsReady() {
			return
		}

		p.lock.RLock()
		onTrackUpdated := p.onTrackUpdated
		p.lock.RUnlock()
		if onTrackUpdated != nil {
			onTrackUpdated(p, track)
		}
	})

	p.UpTrackManager.OnUpTrackManagerClose(p.onUpTrackManagerClose)
}

func (p *ParticipantImpl) updateState(state livekit.ParticipantInfo_State) {
	oldState := p.State()
	if state == oldState {
		return
	}
	p.state.Store(state)
	p.params.Logger.Debugw("updating participant state", "state", state.String())
	p.lock.RLock()
	onStateChange := p.onStateChange
	p.lock.RUnlock()
	if onStateChange != nil {
		go func() {
			defer Recover()
			onStateChange(p, oldState)
		}()
	}
}

// when the server has an offer for participant
func (p *ParticipantImpl) onOffer(offer webrtc.SessionDescription) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		// skip when disconnected
		return
	}

	p.params.Logger.Debugw("sending server offer to participant")

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
		p.reliableDC = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if p.CanPublishData() {
				p.handleDataMessage(livekit.DataPacket_RELIABLE, msg.Data)
			}
		})
	case LossyDataChannel:
		p.lossyDC = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if p.CanPublishData() {
				p.handleDataMessage(livekit.DataPacket_LOSSY, msg.Data)
			}
		})
	default:
		p.params.Logger.Warnw("unsupported datachannel added", nil, "label", dc.Label())
	}
}

func (p *ParticipantImpl) handleDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	dp := livekit.DataPacket{}
	if err := proto.Unmarshal(data, &dp); err != nil {
		p.params.Logger.Warnw("could not parse data packet", err)
		return
	}

	// trust the channel that it came in as the source of truth
	dp.Kind = kind

	// only forward on user payloads
	switch payload := dp.Value.(type) {
	case *livekit.DataPacket_User:
		p.lock.RLock()
		onDataPacket := p.onDataPacket
		p.lock.RUnlock()
		if onDataPacket != nil {
			payload.User.ParticipantSid = string(p.params.SID)
			onDataPacket(p, &dp)
		}
	default:
		p.params.Logger.Warnw("received unsupported data packet", nil, "payload", payload)
	}
}

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
		// clients support resuming of connections when websocket becomes disconnected
		p.closeSignalConnection()

		// detect when participant has actually left.
		go func() {
			p.lock.Lock()
			if p.disconnectTimer != nil {
				p.disconnectTimer.Stop()
			}
			p.disconnectTimer = time.AfterFunc(disconnectCleanupDuration, func() {
				p.lock.Lock()
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
					p.Close(true, types.ParticipantCloseReasonPeerConnectionDisconnected)
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
		// clients support resuming of connections when websocket becomes disconnected
		p.closeSignalConnection()
	}
}

// downTracksRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other publishedTracks in the room.
func (p *ParticipantImpl) downTracksRTCPWorker() {
	defer Recover()
	for {
		time.Sleep(5 * time.Second)

		if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		if p.subscriber.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			continue
		}

		var srs []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		p.lock.RLock()
		for _, subTrack := range p.subscribedTracks {
			sr := subTrack.DownTrack().CreateSenderReport()
			chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
			if sr == nil || chunks == nil {
				continue
			}
			srs = append(srs, sr)
			sd = append(sd, chunks...)
		}
		p.lock.RUnlock()

		// now send in batches of sdBatchSize
		var batch []rtcp.SourceDescriptionChunk
		var pkts []rtcp.Packet
		batchSize := 0
		for len(sd) > 0 || len(srs) > 0 {
			numSRs := len(srs)
			if numSRs > 0 {
				if numSRs > sdBatchSize {
					numSRs = sdBatchSize
				}
				pkts = append(pkts, srs[:numSRs]...)
				srs = srs[numSRs:]
			}

			size := len(sd)
			spaceRemain := sdBatchSize - batchSize
			if spaceRemain > 0 && size > 0 {
				if size > spaceRemain {
					size = spaceRemain
				}
				batch = sd[:size]
				sd = sd[size:]
				pkts = append(pkts, &rtcp.SourceDescription{Chunks: batch})
				if err := p.subscriber.pc.WriteRTCP(pkts); err != nil {
					if err == io.EOF || err == io.ErrClosedPipe {
						return
					}
					logger.Errorw("could not send down track reports", err)
				}
			}

			pkts = pkts[:0]
			batchSize = 0
		}
	}
}

func (p *ParticipantImpl) configureReceiverDTX() {
	//
	// DTX (Discontinuous Transmission) allows audio bandwidth saving
	// by not sending packets during silence periods.
	//
	// Publisher side DTX can enabled by included `usedtx=1` in
	// the `fmtp` line corresponding to audio codec (Opus) in SDP.
	// By doing this in the SDP `answer`, it can be controlled from
	// server side and avoid doing it in all the client SDKs.
	//
	// Ideally, a publisher should be able to specify per audio
	// track if DTX should be enabled. But, translating the
	// DTX preference of publisher to the correct transceiver
	// is non-deterministic due to the lack of a synchronizing id
	// like the track id. The codec preference to set DTX needs
	// to be done
	//   - after calling `SetRemoteDescription` which sets up
	//     the transceivers, but there are no tracks in the
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
	//      to the audio transceiver without no tracks
	//
	// NOTE: The above logic will fail if there is an `offer` SDP with
	// multiple audio tracks. At that point, there might be a need to
	// rely on something like order of tracks. TODO
	//
	enableDTX := p.getDTX()
	transceivers := p.publisher.pc.GetTransceivers()
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
			p.params.Logger.Warnw("failed to SetCodecPreferences", err)
		}
	}
}

func (p *ParticipantImpl) onStreamStateChange(update *sfu.StreamStateUpdate) error {
	if len(update.StreamStates) == 0 {
		return nil
	}

	streamStateUpdate := &livekit.StreamStateUpdate{}
	for _, streamStateInfo := range update.StreamStates {
		state := livekit.StreamState_ACTIVE
		if streamStateInfo.State == sfu.StreamStatePaused {
			state = livekit.StreamState_PAUSED
		}
		streamStateUpdate.StreamStates = append(streamStateUpdate.StreamStates, &livekit.StreamStateInfo{
			ParticipantSid: string(streamStateInfo.ParticipantID),
			TrackSid:       string(streamStateInfo.TrackID),
			State:          state,
		})
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_StreamStateUpdate{
			StreamStateUpdate: streamStateUpdate,
		},
	})
}

func (p *ParticipantImpl) onSubscribedMaxQualityChange(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualites []types.SubscribedCodecQuality) error {
	if len(subscribedQualities) == 0 {
		return nil
	}

	// normalize the codec name
	for _, subscribedQuality := range subscribedQualities {
		subscribedQuality.Codec = strings.ToLower(strings.TrimLeft(subscribedQuality.Codec, "video/"))
	}

	subscribedQualityUpdate := &livekit.SubscribedQualityUpdate{
		TrackSid:            string(trackID),
		SubscribedQualities: subscribedQualities[0].Qualities, // for compatible with old client
		SubscribedCodecs:    subscribedQualities,
	}
	// get track's layer dimensions
	track := p.UpTrackManager.GetPublishedTrack(trackID)
	var layerInfo map[livekit.VideoQuality]*livekit.VideoLayer
	if track != nil {
		layers := track.ToProto().Layers
		layerInfo = make(map[livekit.VideoQuality]*livekit.VideoLayer, len(layers))
		for _, layer := range layers {
			layerInfo[layer.Quality] = layer
		}
	}

	for _, maxSubscribedQuality := range maxSubscribedQualites {
		ti := &livekit.TrackInfo{
			Sid:  string(trackID),
			Type: livekit.TrackType_VIDEO,
		}
		if info, ok := layerInfo[maxSubscribedQuality.Quality]; ok {
			ti.Width = info.Width
			ti.Height = info.Height
		}

		p.params.Telemetry.TrackMaxSubscribedVideoQuality(
			context.Background(),
			p.ID(),
			ti,
			maxSubscribedQuality.CodecMime,
			maxSubscribedQuality.Quality,
		)
	}

	p.params.Logger.Debugw(
		"sending max subscribed quality",
		"trackID", trackID,
		"qualities", subscribedQualities,
		"max", maxSubscribedQualites,
	)
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscribedQualityUpdate{
			SubscribedQualityUpdate: subscribedQualityUpdate,
		},
	})
}

func (p *ParticipantImpl) addPendingTrackLocked(req *livekit.AddTrackRequest) *livekit.TrackInfo {
	if p.getPublishedTrackBySignalCid(req.Cid) != nil || p.getPublishedTrackBySdpCid(req.Cid) != nil {
		return nil
	}

	p.pendingTracksLock.Lock()
	defer p.pendingTracksLock.Unlock()

	// if track is already published, reject
	if p.pendingTracks[req.Cid] != nil {
		return nil
	}

	if req.Sid != "" {
		track := p.getPublishedTrack(livekit.TrackID(req.Sid))
		if track == nil {
			p.params.Logger.Infow("track not found for new codec publish", "trackID", req.Sid)
			return nil
		}

		track.(*MediaTrack).SetPendingCodecSid(req.SimulcastCodecs)
		ti := track.ToProto()
		return ti
	}

	ti := &livekit.TrackInfo{
		Type:       req.Type,
		Name:       req.Name,
		Width:      req.Width,
		Height:     req.Height,
		Muted:      req.Muted,
		DisableDtx: req.DisableDtx,
		Source:     req.Source,
		Layers:     req.Layers,
	}
	p.setStableTrackID(ti)
	pendingInfo := &pendingTrackInfo{TrackInfo: ti}
	for _, codec := range req.SimulcastCodecs {
		mime := codec.Codec
		if req.Type == livekit.TrackType_VIDEO && !strings.HasPrefix(mime, "video/") {
			mime = "video/" + mime
		} else if req.Type == livekit.TrackType_AUDIO && !strings.HasPrefix(mime, "audio/") {
			mime = "audio/" + mime
		}
		ti.Codecs = append(ti.Codecs, &livekit.SimulcastCodecInfo{
			MimeType: mime,
			Cid:      codec.Cid,
		})
	}

	p.pendingTracks[req.Cid] = pendingInfo
	p.params.Logger.Debugw("pending track added", "track", ti.String(), "request", req.String())

	return ti
}

func (p *ParticipantImpl) SetTrackMuted(trackID livekit.TrackID, muted bool, fromAdmin bool) {
	// when request is coming from admin, send message to current participant
	if fromAdmin {
		p.sendTrackMuted(trackID, muted)
	}

	p.setTrackMuted(trackID, muted)
}

func (p *ParticipantImpl) setTrackMuted(trackID livekit.TrackID, muted bool) {
	track := p.UpTrackManager.SetPublishedTrackMuted(trackID, muted)
	if track != nil {
		// handled in UpTrackManager for a published track, no need to update state of pending track
		return
	}

	isPending := false
	p.pendingTracksLock.RLock()
	for _, ti := range p.pendingTracks {
		if livekit.TrackID(ti.Sid) == trackID {
			ti.Muted = muted
			isPending = true
			break
		}
	}
	p.pendingTracksLock.RUnlock()

	if !isPending {
		p.params.Logger.Warnw("could not locate track", nil, "trackID", trackID)
	}
}

func (p *ParticipantImpl) getPublisherConnectionQuality() map[livekit.TrackID]float32 {
	publishedTracks := p.GetPublishedTracks()
	scores := make(map[livekit.TrackID]float32, len(publishedTracks))
	for _, pt := range publishedTracks {
		if pt.IsMuted() {
			continue
		}
		scores[pt.ID()] = pt.(types.LocalMediaTrack).GetConnectionScore()
	}

	return scores
}

func (p *ParticipantImpl) getDTX() bool {
	p.pendingTracksLock.RLock()
	defer p.pendingTracksLock.RUnlock()

	//
	// Although DTX is set per track, there are cases where
	// pending track has to be looked up by kind. This happens
	// when clients change track id between signalling and SDP.
	// In that case, look at all pending tracks by kind and
	// enable DTX even if one has it enabled.
	//
	// Most of the time in practice, there is going to be one
	// audio kind track and hence this is fine.
	//
	for _, ti := range p.pendingTracks {
		if ti.Type == livekit.TrackType_AUDIO {
			if !ti.TrackInfo.DisableDtx {
				return true
			}
		}
	}

	return false
}

func (p *ParticipantImpl) mediaTrackReceived(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) (*MediaTrack, bool) {
	p.pendingTracksLock.Lock()
	newTrack := false

	p.params.Logger.Debugw("media track received", "track", track.ID(), "kind", track.Kind())
	var mid string
	for _, tr := range p.publisher.pc.GetTransceivers() {
		if tr.Receiver() == rtpReceiver {
			mid = tr.Mid()
			break
		}
	}
	// use existing media track to handle simulcast
	mt, ok := p.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti := p.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()))
		if ti == nil {
			p.pendingTracksLock.Unlock()
			return nil, false
		}

		mt = NewMediaTrack(MediaTrackParams{
			TrackInfo:           ti,
			SignalCid:           signalCid,
			SdpCid:              track.ID(),
			ParticipantID:       p.params.SID,
			ParticipantIdentity: p.params.Identity,
			RTCPChan:            p.rtcpCh,
			BufferFactory:       p.params.Config.BufferFactory,
			ReceiverConfig:      p.params.Config.Receiver,
			AudioConfig:         p.params.AudioConfig,
			VideoConfig:         p.params.VideoConfig,
			Telemetry:           p.params.Telemetry,
			Logger:              LoggerWithTrack(p.params.Logger, livekit.TrackID(ti.Sid)),
			SubscriberConfig:    p.params.Config.Subscriber,
			PLIThrottleConfig:   p.params.PLIThrottleConfig,
			SimTracks:           p.params.SimTracks,
		})

		mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)

		// add to published and clean up pending
		p.UpTrackManager.AddPublishedTrack(mt)
		delete(p.pendingTracks, signalCid)

		mt.AddOnClose(func() {
			// re-use track
			p.lock.Lock()
			p.unpublishedTracks = append(p.unpublishedTracks, ti)
			p.lock.Unlock()
		})

		newTrack = true
	}

	ssrc := uint32(track.SSRC())
	if p.twcc == nil {
		p.twcc = twcc.NewTransportWideCCResponder(ssrc)
		p.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			p.postRtcp([]rtcp.Packet{&pkt})
		})
	}
	p.pendingTracksLock.Unlock()

	if mt.AddReceiver(rtpReceiver, track, p.twcc, mid) && newTrack {
		p.handleTrackPublished(mt)
	}

	return mt, newTrack
}

func (p *ParticipantImpl) addMigrateMutedTrack(cid string, t *livekit.TrackInfo) *MediaTrack {
	p.params.Logger.Debugw("add migrate muted track", "cid", cid, "track", t.String())
	var rtpReceiver *webrtc.RTPReceiver
	for _, tr := range p.publisher.pc.GetTransceivers() {
		if tr.Mid() == t.Mid {
			rtpReceiver = tr.Receiver()
			break
		}
	}
	if rtpReceiver == nil {
		p.params.Logger.Errorw("could not find receiver for migrated track", nil, "track", t.Sid)
		return nil
	}

	mt := NewMediaTrack(MediaTrackParams{
		TrackInfo:           proto.Clone(t).(*livekit.TrackInfo),
		SignalCid:           cid,
		SdpCid:              cid,
		ParticipantID:       p.params.SID,
		ParticipantIdentity: p.params.Identity,
		RTCPChan:            p.rtcpCh,
		BufferFactory:       p.params.Config.BufferFactory,
		ReceiverConfig:      p.params.Config.Receiver,
		AudioConfig:         p.params.AudioConfig,
		VideoConfig:         p.params.VideoConfig,
		Telemetry:           p.params.Telemetry,
		Logger:              LoggerWithTrack(p.params.Logger, livekit.TrackID(t.Sid)),
		SubscriberConfig:    p.params.Config.Subscriber,
		PLIThrottleConfig:   p.params.PLIThrottleConfig,
		SimTracks:           p.params.SimTracks,
	})

	mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)
	// add to published and clean up pending
	p.UpTrackManager.AddPublishedTrack(mt)
	delete(p.pendingTracks, cid)

	mt.AddOnClose(func() {
		// re-use track
		p.lock.Lock()
		p.unpublishedTracks = append(p.unpublishedTracks, t)
		p.lock.Unlock()
	})

	potentialCodecs := make([]webrtc.RTPCodecParameters, 0, len(t.Codecs))
	parameters := rtpReceiver.GetParameters()
	for _, c := range t.Codecs {
		for _, nc := range parameters.Codecs {
			if strings.EqualFold(nc.MimeType, c.MimeType) {
				potentialCodecs = append(potentialCodecs, nc)
				break
			}
		}
	}
	mt.SetPotentialCodecs(potentialCodecs, parameters.HeaderExtensions)

	for _, codec := range t.Codecs {
		for ssrc, info := range p.params.SimTracks {
			if info.Mid == codec.Mid {
				mt.MediaTrackReceiver.SetLayerSsrc(codec.MimeType, info.Rid, ssrc)
			}
		}
	}
	mt.SetSimulcast(t.Simulcast)
	mt.SetMuted(true)

	return mt
}

func (p *ParticipantImpl) handleTrackPublished(track types.MediaTrack) {
	if !p.hasPendingMigratedTrack() {
		p.SetMigrateState(types.MigrateStateComplete)
	}

	if p.onTrackPublished != nil {
		p.onTrackPublished(p, track)
	}
}

func (p *ParticipantImpl) hasPendingMigratedTrack() bool {
	p.pendingTracksLock.RLock()
	defer p.pendingTracksLock.RUnlock()

	for _, t := range p.pendingTracks {
		if t.migrated {
			return true
		}
	}

	return false
}

func (p *ParticipantImpl) onUpTrackManagerClose() {
	p.postRtcp(nil)
}

func (p *ParticipantImpl) getPendingTrack(clientId string, kind livekit.TrackType) (string, *livekit.TrackInfo) {
	signalCid := clientId
	trackInfo := p.pendingTracks[clientId]
	if trackInfo == nil {
	track_loop:
		for cid, ti := range p.pendingTracks {
			if cid == clientId {
				trackInfo = ti
				signalCid = cid
				break
			}

			for _, c := range ti.Codecs {
				if c.Cid == clientId {
					trackInfo = ti
					signalCid = cid
					break track_loop
				}
			}
		}

		if trackInfo == nil {
			//
			// If no match on client id, find first one matching type
			// as MediaStreamTrack can change client id when transceiver
			// is added to peer connection.
			//
			for cid, ti := range p.pendingTracks {
				if ti.Type == kind {
					trackInfo = ti
					signalCid = cid
					break
				}
			}
		}
	}

	// if still not found, we are done
	if trackInfo == nil {
		p.params.Logger.Errorw("track info not published prior to track", nil, "clientId", clientId)
		return signalCid, nil
	}

	return signalCid, trackInfo.TrackInfo
}

// setStableTrackID either generates a new TrackID or reuses a previously used one
// for
func (p *ParticipantImpl) setStableTrackID(info *livekit.TrackInfo) {
	var trackID string
	for i, ti := range p.unpublishedTracks {
		if ti.Type == info.Type && ti.Source == info.Source && ti.Name == info.Name {
			trackID = ti.Sid
			if i < len(p.unpublishedTracks)-1 {
				p.unpublishedTracks = append(p.unpublishedTracks[:i], p.unpublishedTracks[i+1:]...)
			} else {
				p.unpublishedTracks = p.unpublishedTracks[:i]
			}
			break
		}
	}
	// otherwise generate
	if trackID == "" {
		trackPrefix := utils.TrackPrefix
		if info.Type == livekit.TrackType_VIDEO {
			trackPrefix += "V"
		} else if info.Type == livekit.TrackType_AUDIO {
			trackPrefix += "A"
		}
		switch info.Source {
		case livekit.TrackSource_CAMERA:
			trackPrefix += "C"
		case livekit.TrackSource_MICROPHONE:
			trackPrefix += "M"
		case livekit.TrackSource_SCREEN_SHARE:
			trackPrefix += "S"
		case livekit.TrackSource_SCREEN_SHARE_AUDIO:
			trackPrefix += "s"
		}
		trackID = utils.NewGuid(trackPrefix)
	}
	info.Sid = trackID
}

func (p *ParticipantImpl) getPublishedTrackBySignalCid(clientId string) types.MediaTrack {
	for _, publishedTrack := range p.GetPublishedTracks() {
		if publishedTrack.(types.LocalMediaTrack).SignalCid() == clientId {
			return publishedTrack
		}
	}

	return nil
}

func (p *ParticipantImpl) getPublishedTrackBySdpCid(clientId string) types.MediaTrack {
	for _, publishedTrack := range p.GetPublishedTracks() {
		if publishedTrack.(types.LocalMediaTrack).HasSdpCid(clientId) {
			p.params.Logger.Debugw("found track by sdp cid", "sdpCid", clientId, "trackID", publishedTrack.ID())
			return publishedTrack
		}
	}

	return nil
}

func (p *ParticipantImpl) rtcpSendWorker() {
	defer Recover()

	// read from rtcpChan
	for pkts := range p.rtcpCh {
		if pkts == nil {
			return
		}

		if err := p.publisher.pc.WriteRTCP(pkts); err != nil {
			p.params.Logger.Errorw("could not write RTCP to participant", err)
		}
	}
}

func (p *ParticipantImpl) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"ID":    p.params.SID,
		"State": p.State().String(),
	}

	pendingTrackInfo := make(map[string]interface{})
	p.pendingTracksLock.RLock()
	for clientID, ti := range p.pendingTracks {
		pendingTrackInfo[clientID] = map[string]interface{}{
			"Sid":       ti.Sid,
			"Type":      ti.Type.String(),
			"Simulcast": ti.Simulcast,
		}
	}
	p.pendingTracksLock.RUnlock()
	info["PendingTracks"] = pendingTrackInfo

	info["UpTrackManager"] = p.UpTrackManager.DebugInfo()

	subscribedTrackInfo := make(map[livekit.TrackID]interface{})
	p.lock.RLock()
	for _, track := range p.subscribedTracks {
		dt := track.DownTrack().DebugInfo()
		dt["SubMuted"] = track.IsMuted()
		subscribedTrackInfo[track.ID()] = dt
	}
	p.lock.RUnlock()
	info["SubscribedTracks"] = subscribedTrackInfo

	return info
}

func (p *ParticipantImpl) handlePendingPublisherDataChannels() {
	ordered := true
	negotiated := true
	for _, ci := range p.pendingDataChannels {
		var (
			dc  *webrtc.DataChannel
			err error
		)
		if ci.Target == livekit.SignalTarget_SUBSCRIBER {
			continue
		}
		if ci.Label == LossyDataChannel && p.lossyDC == nil {
			retransmits := uint16(0)
			id := uint16(ci.GetId())
			dc, err = p.publisher.pc.CreateDataChannel(LossyDataChannel, &webrtc.DataChannelInit{
				Ordered:        &ordered,
				MaxRetransmits: &retransmits,
				Negotiated:     &negotiated,
				ID:             &id,
			})
		} else if ci.Label == ReliableDataChannel && p.reliableDC == nil {
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
	p.pendingDataChannels = nil
}

func (p *ParticipantImpl) GetSubscribedTracks() []types.SubscribedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()

	tracks := make([]types.SubscribedTrack, 0, len(p.subscribedTracks))
	for _, t := range p.subscribedTracks {
		tracks = append(tracks, t)
	}
	return tracks
}

func (p *ParticipantImpl) incActiveCounter() {
	if p.activeCounter.Inc() == stateActiveCond {
		p.updateState(livekit.ParticipantInfo_ACTIVE)
	}
}

func (p *ParticipantImpl) postRtcp(pkts []rtcp.Packet) {
	select {
	case p.rtcpCh <- pkts:
	default:
		p.params.Logger.Warnw("rtcp channel full", nil)
	}
}

func (p *ParticipantImpl) setDowntracksConnected() {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, t := range p.subscribedTracks {
		if dt := t.DownTrack(); dt != nil {
			dt.SetConnected()
		}
	}
}

func (p *ParticipantImpl) CacheDownTrack(trackID livekit.TrackID, rtpTransceiver *webrtc.RTPTransceiver, forwarderState sfu.ForwarderState) {
	p.lock.Lock()
	if existing := p.cachedDownTracks[trackID]; existing != nil && existing.transceiver != rtpTransceiver {
		p.params.Logger.Infow("cached transceiver change", "trackID", trackID)
	}
	p.cachedDownTracks[trackID] = &downTrackState{transceiver: rtpTransceiver, forwarder: forwarderState}
	p.lock.Unlock()
}

func (p *ParticipantImpl) UncacheDownTrack(rtpTransceiver *webrtc.RTPTransceiver) {
	p.lock.Lock()
	for trackID, dts := range p.cachedDownTracks {
		if dts.transceiver == rtpTransceiver {
			delete(p.cachedDownTracks, trackID)
			break
		}
	}
	p.lock.Unlock()
}

func (p *ParticipantImpl) GetCachedDownTrack(trackID livekit.TrackID) (*webrtc.RTPTransceiver, sfu.ForwarderState) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	dts := p.cachedDownTracks[trackID]
	if dts != nil {
		return dts.transceiver, dts.forwarder
	}

	return nil, sfu.ForwarderState{}
}

func (p *ParticipantImpl) handleNegotiationFailed() {
	p.params.Logger.Infow("negotiation failed, notify client do full reconnect")
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
