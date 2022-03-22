package rtc

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
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
)

const (
	LossyDataChannel    = "_lossy"
	ReliableDataChannel = "_reliable"

	sdBatchSize       = 20
	rttUpdateInterval = 5 * time.Second

	stateActiveCond           = 3 // reliableDCOpen,lossyDCOpen,PeerConnectionStateConnected
	initNetWorkCost           = 100
	disconnectCleanupDuration = 15 * time.Second
)

type pendingTrackInfo struct {
	*livekit.TrackInfo
	migrated bool
}

type ParticipantParams struct {
	Identity                livekit.ParticipantIdentity
	Name                    livekit.ParticipantName
	SID                     livekit.ParticipantID
	Config                  *WebRTCConfig
	Sink                    routing.MessageSink
	AudioConfig             config.AudioConfig
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
	grants              atomic.Value // *auth.ClaimGrants

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

	// tracks the current participant is subscribed to, map of trackID => types.SubscribedTrack
	subscribedTracks map[livekit.TrackID]types.SubscribedTrack
	// track settings of tracks the current participant is subscribed to, map of trackID => types.SubscribedTrack
	subscribedTracksSettings map[livekit.TrackID]*livekit.UpdateTrackSettings
	// keeps track of disallowed tracks
	disallowedSubscriptions map[livekit.TrackID]livekit.ParticipantID // trackID -> publisherID
	// keep track of other publishers identities that we are subscribed to
	subscribedTo sync.Map // livekit.ParticipantID => struct{}

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

	migrateState        atomic.Value // types.MigrateState
	pendingOffer        *webrtc.SessionDescription
	pendingDataChannels []*livekit.DataChannelInfo
	onClose             func(types.LocalParticipant, map[livekit.TrackID]livekit.ParticipantID)
	onClaimsChanged     func(participant types.LocalParticipant)

	activeCounter atomic.Int32
	networkCost   int32
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
		rtcpCh:                   make(chan []rtcp.Packet, 50),
		pendingTracks:            make(map[string]*pendingTrackInfo),
		subscribedTracks:         make(map[livekit.TrackID]types.SubscribedTrack),
		subscribedTracksSettings: make(map[livekit.TrackID]*livekit.UpdateTrackSettings),
		disallowedSubscriptions:  make(map[livekit.TrackID]livekit.ParticipantID),
		connectedAt:              time.Now(),
		rttUpdatedAt:             time.Now(),
		networkCost:              initNetWorkCost,
	}
	p.version.Store(params.InitialVersion)
	p.migrateState.Store(types.MigrateStateInit)
	p.state.Store(livekit.ParticipantInfo_JOINING)
	p.grants.Store(params.Grants)
	p.SetResponseSink(params.Sink)

	var err error
	// keep last participants and when updates were sent
	if p.updateCache, err = lru.New(32); err != nil {
		return nil, err
	}
	p.publisher, err = NewPCTransport(TransportParams{
		ParticipantID:           p.params.SID,
		ParticipantIdentity:     p.params.Identity,
		ProtocolVersion:         p.ProtocolVersion(),
		Target:                  livekit.SignalTarget_PUBLISHER,
		Config:                  params.Config,
		CongestionControlConfig: params.CongestionControlConfig,
		Telemetry:               p.params.Telemetry,
		EnabledCodecs:           p.params.EnabledCodecs,
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
		EnabledCodecs:           p.params.EnabledCodecs,
		Logger:                  LoggerWithPCTarget(params.Logger, livekit.SignalTarget_SUBSCRIBER),
	})
	if err != nil {
		return nil, err
	}

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
		ordered := true
		// also create data channels for subs, this is for legacy clients that do not use subscriber
		// as primary channel
		p.reliableDCSub, err = primaryPC.CreateDataChannel(ReliableDataChannel, &webrtc.DataChannelInit{
			Ordered: &ordered,
		})
		if err != nil {
			return nil, err
		}
		p.reliableDCSub.OnOpen(p.incActiveCounter)
		retransmits := uint16(0)
		p.lossyDCSub, err = primaryPC.CreateDataChannel(LossyDataChannel, &webrtc.DataChannelInit{
			Ordered:        &ordered,
			MaxRetransmits: &retransmits,
		})
		if err != nil {
			return nil, err
		}
		p.lossyDCSub.OnOpen(p.incActiveCounter)
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

func (p *ParticipantImpl) GetLogger() logger.Logger {
	return p.params.Logger
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
	grants := p.ClaimGrants()
	changed := grants.Metadata != metadata
	grants.Metadata = metadata
	p.grants.Store(grants)

	if !changed {
		return
	}

	if p.onParticipantUpdate != nil {
		p.onParticipantUpdate(p)
	}
	if p.onClaimsChanged != nil {
		p.onClaimsChanged(p)
	}
}

func (p *ParticipantImpl) ClaimGrants() *auth.ClaimGrants {
	return p.grants.Load().(*auth.ClaimGrants)
}

func (p *ParticipantImpl) SetPermission(permission *livekit.ParticipantPermission) bool {
	if permission == nil {
		return false
	}
	grants := p.ClaimGrants()
	video := grants.Video
	hasChanged := video.GetCanSubscribe() != permission.CanSubscribe ||
		video.GetCanPublish() != permission.CanPublish ||
		video.GetCanPublishData() != permission.CanPublishData ||
		video.Hidden != permission.Hidden ||
		video.Recorder != permission.Recorder

	if !hasChanged {
		return false
	}

	video.SetCanSubscribe(permission.CanSubscribe)
	video.SetCanPublish(permission.CanPublish)
	video.SetCanPublishData(permission.CanPublishData)
	video.Hidden = permission.Hidden
	video.Recorder = permission.Recorder
	p.grants.Store(grants)

	// publish permission has been revoked then remove all published tracks
	if !video.GetCanPublish() {
		for _, track := range p.GetPublishedTracks() {
			p.RemovePublishedTrack(track)
			if p.ProtocolVersion().SupportsUnpublish() {
				p.sendTrackUnpublished(track.ID())
			} else {
				// for older clients that don't support unpublish, mute to avoid them sending data
				p.sendTrackMuted(track.ID(), true)
			}
		}
	}

	if p.onParticipantUpdate != nil {
		p.onParticipantUpdate(p)
	}
	if p.onClaimsChanged != nil {
		p.onClaimsChanged(p)
	}
	return true
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	grants := p.ClaimGrants()
	info := &livekit.ParticipantInfo{
		Sid:        string(p.params.SID),
		Identity:   string(p.params.Identity),
		Name:       string(p.params.Name),
		State:      p.State(),
		JoinedAt:   p.ConnectedAt().Unix(),
		Version:    p.version.Inc(),
		Permission: grants.Video.ToPermission(),
	}
	info.Tracks = p.UpTrackManager.ToProto()
	if p.params.Grants != nil {
		info.Metadata = grants.Metadata
	}

	return info
}

func (p *ParticipantImpl) SubscriberMediaEngine() *webrtc.MediaEngine {
	return p.subscriber.me
}

// callbacks for clients

func (p *ParticipantImpl) OnTrackPublished(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.onTrackPublished = callback
}

func (p *ParticipantImpl) OnStateChange(callback func(p types.LocalParticipant, oldState livekit.ParticipantInfo_State)) {
	p.onStateChange = callback
}

func (p *ParticipantImpl) OnTrackUpdated(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.onTrackUpdated = callback
}

func (p *ParticipantImpl) OnParticipantUpdate(callback func(types.LocalParticipant)) {
	p.onParticipantUpdate = callback
}

func (p *ParticipantImpl) OnDataPacket(callback func(types.LocalParticipant, *livekit.DataPacket)) {
	p.onDataPacket = callback
}

func (p *ParticipantImpl) OnClose(callback func(types.LocalParticipant, map[livekit.TrackID]livekit.ParticipantID)) {
	p.onClose = callback
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

	if p.State() == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	prometheus.ServiceOperationCounter.WithLabelValues("answer", "success", "").Add(1)

	return
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (p *ParticipantImpl) AddTrack(req *livekit.AddTrackRequest) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.CanPublish() {
		p.params.Logger.Warnw("no permission to publish track", nil)
		return
	}

	ti := p.addPendingTrack(req)
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

func (p *ParticipantImpl) SetMigrateInfo(mediaTracks []*livekit.TrackPublishedResponse, dataChannels []*livekit.DataChannelInfo) {
	p.pendingTracksLock.Lock()
	defer p.pendingTracksLock.Unlock()

	for _, t := range mediaTracks {
		p.pendingTracks[t.GetCid()] = &pendingTrackInfo{t.GetTrack(), true}
	}
	p.pendingDataChannels = dataChannels
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

func (p *ParticipantImpl) Close(sendLeave bool) error {
	if p.isClosed.Swap(true) {
		// already closed
		return nil
	}

	// send leave message
	if sendLeave {
		_ = p.writeMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{
				Leave: &livekit.LeaveRequest{},
			},
		})
	}

	p.UpTrackManager.Close()

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
	p.publisher.Close()
	p.subscriber.Close()
	return nil
}

func (p *ParticipantImpl) Negotiate() {
	if p.MigrateState() != types.MigrateStateInit {
		p.subscriber.Negotiate()
	}
}

func (p *ParticipantImpl) SetPreviousAnswer(previous *webrtc.SessionDescription) {
	p.subscriber.SetPreviousAnswer(previous)
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
		if !p.hasPendingMigratedTrack() {
			p.migrateState.Store(types.MigrateStateComplete)
		}
		pendingOffer = p.pendingOffer
		p.pendingOffer = nil
		// in case of migration, subscriber data channel will not fire OnOpen callback
		p.activeCounter.CAS(0, 2)

		// use lower network cost to make sure migrated's node has higher priority candidate
		p.networkCost--
	}
	p.lock.Unlock()
	if s == types.MigrateStateComplete {
		p.handlePendingDataChannels()
	}
	if pendingOffer != nil {
		p.HandleOffer(*pendingOffer)
	}
}

func (p *ParticipantImpl) MigrateState() types.MigrateState {
	return p.migrateState.Load().(types.MigrateState)
}

// ICERestart restarts subscriber ICE connections
func (p *ParticipantImpl) ICERestart() error {
	if p.subscriber.pc.RemoteDescription() == nil {
		// not connected, skip
		return nil
	}
	return p.subscriber.CreateAndSendOffer(&webrtc.OfferOptions{
		ICERestart: true,
	})
}

//
// signal connection methods
//

func (p *ParticipantImpl) GetAudioLevel() (level uint8, active bool) {
	level = SilentAudioLevel
	for _, pt := range p.GetPublishedTracks() {
		tl, ta := pt.(types.LocalMediaTrack).GetAudioLevel()
		if ta {
			active = true
			if tl < level {
				level = tl
			}
		}
	}
	return
}

func (p *ParticipantImpl) GetConnectionQuality() *livekit.ConnectionQualityInfo {
	// avg loss across all tracks, weigh published the same as subscribed
	totalScore, numTracks := p.getPublisherConnectionQuality()

	p.lock.RLock()
	for _, subTrack := range p.subscribedTracks {
		if subTrack.IsMuted() || subTrack.MediaTrack().IsMuted() {
			continue
		}
		totalScore += subTrack.DownTrack().GetConnectionScore()
		numTracks++
	}
	p.lock.RUnlock()

	avgScore := float32(5.0)
	if numTracks > 0 {
		avgScore = totalScore / float32(numTracks)
	}

	rating := connectionquality.Score2Rating(avgScore)

	return &livekit.ConnectionQualityInfo{
		ParticipantSid: string(p.ID()),
		Quality:        rating,
		Score:          float32(avgScore),
	}
}

func (p *ParticipantImpl) GetSubscribedParticipants() []livekit.ParticipantID {
	var participantIDs []livekit.ParticipantID
	p.subscribedTo.Range(func(key, _ interface{}) bool {
		if participantID, ok := key.(livekit.ParticipantID); ok {
			participantIDs = append(participantIDs, participantID)
		}
		return true
	})
	return participantIDs
}

func (p *ParticipantImpl) CanPublish() bool {
	return p.ClaimGrants().Video.GetCanPublish()
}

func (p *ParticipantImpl) CanSubscribe() bool {
	return p.ClaimGrants().Video.GetCanSubscribe()
}

func (p *ParticipantImpl) CanPublishData() bool {
	return p.ClaimGrants().Video.GetCanPublishData()
}

func (p *ParticipantImpl) Hidden() bool {
	return p.ClaimGrants().Video.Hidden
}

func (p *ParticipantImpl) IsRecorder() bool {
	return p.ClaimGrants().Video.Recorder
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
	p.subscribedTracks[subTrack.ID()] = subTrack
	settings := p.subscribedTracksSettings[subTrack.ID()]
	p.lock.Unlock()

	subTrack.OnBind(func() {
		p.subscriber.AddTrack(subTrack)
	})

	if settings != nil {
		subTrack.UpdateSubscriberSettings(settings)
	}

	p.subscribedTo.Store(subTrack.PublisherID(), struct{}{})
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
	// subscribedTrackSettings should not deleted on removal as it is needed if corresponding publisher migrated
	// LK-TODO: find a way to clean these up
	//

	p.lock.Unlock()

	if numRemaining == 0 {
		p.subscribedTo.Delete(subTrack.PublisherID())

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

		if p.onTrackUpdated != nil {
			p.onTrackUpdated(p, track)
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

	p.params.Logger.Infow("mediaTrack published",
		"kind", track.Kind().String(),
		"trackID", publishedTrack.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC())
	if !isNewTrack && publishedTrack != nil && p.IsReady() && p.onTrackUpdated != nil {
		p.onTrackUpdated(p, publishedTrack)
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
		if p.onDataPacket != nil {
			payload.User.ParticipantSid = string(p.params.SID)
			p.onDataPacket(p, &dp)
		}
	default:
		p.params.Logger.Warnw("received unsupported data packet", nil, "payload", payload)
	}
}

func (p *ParticipantImpl) handlePrimaryStateChange(state webrtc.PeerConnectionState) {
	if state == webrtc.PeerConnectionStateConnected {
		prometheus.ServiceOperationCounter.WithLabelValues("ice_connection", "success", "").Add(1)
		if !p.hasPendingMigratedTrack() && p.MigrateState() == types.MigrateStateSync {
			p.SetMigrateState(types.MigrateStateComplete)
		}
		p.incActiveCounter()
	} else if state == webrtc.PeerConnectionStateDisconnected {
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
					p.Close(true)
				}
			})
			p.lock.Unlock()
		}()

	}
}

// for the secondary peer connection, we still need to handle when they become disconnected
// instead of allowing them to silently fail.
func (p *ParticipantImpl) handleSecondaryStateChange(state webrtc.PeerConnectionState) {
	if state == webrtc.PeerConnectionStateDisconnected {
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

func (p *ParticipantImpl) isSubscribedTo(participantID livekit.ParticipantID) bool {
	_, ok := p.subscribedTo.Load(participantID)
	return ok
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

func (p *ParticipantImpl) onSubscribedMaxQualityChange(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality) error {
	if len(subscribedQualities) == 0 {
		return nil
	}

	subscribedQualityUpdate := &livekit.SubscribedQualityUpdate{
		TrackSid:            string(trackID),
		SubscribedQualities: subscribedQualities,
	}

	p.params.Telemetry.TrackMaxSubscribedVideoQuality(context.Background(), p.ID(), &livekit.TrackInfo{Sid: string(trackID), Type: livekit.TrackType_VIDEO}, maxSubscribedQuality)

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscribedQualityUpdate{
			SubscribedQualityUpdate: subscribedQualityUpdate,
		},
	})
}

func (p *ParticipantImpl) addPendingTrack(req *livekit.AddTrackRequest) *livekit.TrackInfo {
	if p.getPublishedTrackBySignalCid(req.Cid) != nil || p.getPublishedTrackBySdpCid(req.Cid) != nil {
		return nil
	}

	p.pendingTracksLock.Lock()
	defer p.pendingTracksLock.Unlock()

	// if track is already published, reject
	if p.pendingTracks[req.Cid] != nil {
		return nil
	}

	ti := &livekit.TrackInfo{
		Type:       req.Type,
		Name:       req.Name,
		Sid:        utils.NewGuid(utils.TrackPrefix),
		Width:      req.Width,
		Height:     req.Height,
		Muted:      req.Muted,
		DisableDtx: req.DisableDtx,
		Source:     req.Source,
		Layers:     req.Layers,
	}
	p.pendingTracks[req.Cid] = &pendingTrackInfo{TrackInfo: ti}

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

func (p *ParticipantImpl) getPublisherConnectionQuality() (totalScore float32, numTracks int) {
	for _, pt := range p.GetPublishedTracks() {
		if pt.IsMuted() {
			continue
		}
		totalScore += pt.(types.LocalMediaTrack).GetConnectionScore()
		numTracks++
	}

	return
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

func (p *ParticipantImpl) mediaTrackReceived(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) (types.MediaTrack, bool) {
	p.pendingTracksLock.Lock()
	newTrack := false

	// use existing mediatrack to handle simulcast
	mt, ok := p.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti := p.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()))
		if ti == nil {
			p.pendingTracksLock.Unlock()
			return nil, false
		}

		ti.MimeType = track.Codec().MimeType

		var mid string
		for _, tr := range p.publisher.pc.GetTransceivers() {
			if tr.Receiver() == rtpReceiver {
				mid = tr.Mid()
				break
			}
		}
		ti.Mid = mid

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
			Telemetry:           p.params.Telemetry,
			Logger:              LoggerWithTrack(p.params.Logger, livekit.TrackID(ti.Sid)),
			SubscriberConfig:    p.params.Config.Subscriber,
			PLIThrottleConfig:   p.params.PLIThrottleConfig,
		})

		for ssrc, info := range p.params.SimTracks {
			if info.Mid == mid {
				mt.TrySetSimulcastSSRC(uint8(sfu.RidToLayer(info.Rid)), ssrc)
			}
		}

		mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)

		// add to published and clean up pending
		p.UpTrackManager.AddPublishedTrack(mt)
		delete(p.pendingTracks, signalCid)

		newTrack = true
	}

	ssrc := uint32(track.SSRC())
	if p.twcc == nil {
		p.twcc = twcc.NewTransportWideCCResponder(ssrc)
		p.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			if err := p.publisher.pc.WriteRTCP([]rtcp.Packet{&pkt}); err != nil {
				p.params.Logger.Errorw("could not write RTCP to participant", err)
			}
		})
	}
	p.pendingTracksLock.Unlock()

	mt.AddReceiver(rtpReceiver, track, p.twcc)

	if newTrack {
		p.handleTrackPublished(mt)
	}

	return mt, newTrack
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
	p.rtcpCh <- nil
}

func (p *ParticipantImpl) getPendingTrack(clientId string, kind livekit.TrackType) (string, *livekit.TrackInfo) {
	signalCid := clientId
	trackInfo := p.pendingTracks[clientId]

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

	// if still not found, we are done
	if trackInfo == nil {
		p.params.Logger.Errorw("track info not published prior to track", nil, "clientId", clientId)
		return signalCid, nil
	}

	return signalCid, trackInfo.TrackInfo
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
		if publishedTrack.(types.LocalMediaTrack).SdpCid() == clientId {
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

func (p *ParticipantImpl) handlePendingDataChannels() {
	ordered := true
	negotiated := true
	for _, ci := range p.pendingDataChannels {
		var (
			dc  *webrtc.DataChannel
			err error
		)
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
