package rtc

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/supervisor"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

const (
	sdBatchSize       = 20
	rttUpdateInterval = 5 * time.Second

	disconnectCleanupDuration = 15 * time.Second
	migrationWaitDuration     = 3 * time.Second
)

type pendingTrackInfo struct {
	trackInfos []*livekit.TrackInfo
	migrated   bool
}

type downTrackState struct {
	transceiver *webrtc.RTPTransceiver
	downTrack   sfu.DownTrackState
}

type SubscribeRequestType int

const (
	SubscribeRequestTypeRemove SubscribeRequestType = iota
	SubscribeRequestTypeAdd
)

type SubscribeRequest struct {
	requestType   SubscribeRequestType
	willBeResumed bool
	addCb         func(sub types.LocalParticipant) error
	removeCb      func(subscriberID livekit.ParticipantID, willBeResumed bool) error
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
	ClientInfo              ClientInfo
	Region                  string
	Migration               bool
	AdaptiveStream          bool
	AllowTCPFallback        bool
	TURNSEnabled            bool
	GetParticipantInfo      func(pID livekit.ParticipantID) *livekit.ParticipantInfo
}

type ParticipantImpl struct {
	params ParticipantParams

	isClosed     atomic.Bool
	state        atomic.Value // livekit.ParticipantInfo_State
	updateCache  *lru.Cache
	resSink      atomic.Value // routing.MessageSink
	resSinkValid atomic.Bool
	grants       *auth.ClaimGrants
	isPublisher  atomic.Bool

	// when first connected
	connectedAt time.Time
	// timer that's set when disconnect is detected on primary PC
	disconnectTimer *time.Timer
	migrationTimer  *time.Timer

	rtcpCh chan []rtcp.Packet

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// client intended to publish, yet to be reconciled
	pendingTracksLock sync.RWMutex
	pendingTracks     map[string]*pendingTrackInfo

	*TransportManager
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

	migrateState atomic.Value // types.MigrateState

	onClose            func(types.LocalParticipant, map[livekit.TrackID]livekit.ParticipantID)
	onClaimsChanged    func(participant types.LocalParticipant)
	onICEConfigChanged func(participant types.LocalParticipant, iceConfig types.IceConfig)

	cachedDownTracks map[livekit.TrackID]*downTrackState

	subscriptionInProgress    map[livekit.TrackID]bool
	subscriptionRequestsQueue map[livekit.TrackID][]SubscribeRequest
	trackPublisherVersion     map[livekit.TrackID]uint32

	supervisor *supervisor.ParticipantSupervisor
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
		params:                    params,
		rtcpCh:                    make(chan []rtcp.Packet, 100),
		pendingTracks:             make(map[string]*pendingTrackInfo),
		subscribedTracks:          make(map[livekit.TrackID]types.SubscribedTrack),
		subscribedTracksSettings:  make(map[livekit.TrackID]*livekit.UpdateTrackSettings),
		disallowedSubscriptions:   make(map[livekit.TrackID]livekit.ParticipantID),
		subscribedTo:              make(map[livekit.ParticipantID]struct{}),
		connectedAt:               time.Now(),
		rttUpdatedAt:              time.Now(),
		cachedDownTracks:          make(map[livekit.TrackID]*downTrackState),
		subscriptionInProgress:    make(map[livekit.TrackID]bool),
		subscriptionRequestsQueue: make(map[livekit.TrackID][]SubscribeRequest),
		trackPublisherVersion:     make(map[livekit.TrackID]uint32),
		supervisor:                supervisor.NewParticipantSupervisor(supervisor.ParticipantSupervisorParams{Logger: params.Logger}),
	}
	p.version.Store(params.InitialVersion)
	p.migrateState.Store(types.MigrateStateInit)
	p.state.Store(livekit.ParticipantInfo_JOINING)
	p.grants = params.Grants
	p.SetResponseSink(params.Sink)

	var err error
	// keep last participants and when updates were sent
	if p.updateCache, err = lru.New(128); err != nil {
		return nil, err
	}

	err = p.setupTransportManager()
	if err != nil {
		return nil, err
	}

	p.setupUpTrackManager()

	return p, nil
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

func (p *ParticipantImpl) GetClientConfiguration() *livekit.ClientConfiguration {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.params.ClientConf
}

func (p *ParticipantImpl) GetICEConnectionType() types.ICEConnectionType {
	return p.TransportManager.GetICEConnectionType()
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
			p.RemovePublishedTrack(track, false, false)
			if p.ProtocolVersion().SupportsUnpublish() {
				p.sendTrackUnpublished(track.ID())
			} else {
				// for older clients that don't support unpublish, mute to avoid them sending data
				p.sendTrackMuted(track.ID(), true)
			}
		}
	}
	// update isPublisher attribute
	p.isPublisher.Store(canPublish && p.TransportManager.IsPublisherEstablished())

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
	p.lock.Lock()
	p.onClaimsChanged = callback
	p.lock.Unlock()
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(offer webrtc.SessionDescription) {
	p.params.Logger.Infow("received offer", "transport", livekit.SignalTarget_PUBLISHER)
	shouldPend := false
	if p.MigrateState() == types.MigrateStateInit {
		shouldPend = true
	}

	offer = p.setCodecPreferencesForPublisher(offer)

	p.TransportManager.HandleOffer(offer, shouldPend)
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (p *ParticipantImpl) HandleAnswer(answer webrtc.SessionDescription) {
	p.params.Logger.Infow("received answer", "transport", livekit.SignalTarget_SUBSCRIBER)

	/* from server received join request to client answer
	 * 1. server send join response & offer
	 * ... swap candidates
	 * 2. client send answer
	 */
	signalConnCost := time.Since(p.ConnectedAt()).Milliseconds()
	p.TransportManager.UpdateRTT(uint32(signalConnCost), false)

	p.TransportManager.HandleAnswer(answer)
}

func (p *ParticipantImpl) onPublisherAnswer(answer webrtc.SessionDescription) error {
	p.params.Logger.Infow("sending answer", "transport", livekit.SignalTarget_PUBLISHER)
	answer = p.configurePublisherAnswer(answer)
	if err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	}); err != nil {
		return err
	}

	if p.isPublisher.Load() != p.CanPublish() {
		p.isPublisher.Store(p.CanPublish())
		// trigger update as well if participant is already fully connected
		if p.State() == livekit.ParticipantInfo_ACTIVE {
			p.lock.RLock()
			onParticipantUpdate := p.onParticipantUpdate
			p.lock.RUnlock()

			if onParticipantUpdate != nil {
				onParticipantUpdate(p)
			}
		}
	}

	if p.MigrateState() == types.MigrateStateSync {
		go p.handleMigrateMutedTrack()
	}
	return nil
}

func (p *ParticipantImpl) handleMigrateMutedTrack() {
	// muted track won't send rtp packet, so we add mediatrack manually
	var addedTracks []*MediaTrack
	p.pendingTracksLock.Lock()
	for cid, pti := range p.pendingTracks {
		if !pti.migrated {
			continue
		}

		if len(pti.trackInfos) > 1 {
			p.params.Logger.Warnw("too many pending migrated tracks", nil, "count", len(pti.trackInfos), "cid", cid)
		}

		ti := pti.trackInfos[0]
		if ti.Muted && ti.Type == livekit.TrackType_VIDEO {
			mt := p.addMigrateMutedTrack(cid, ti)
			if mt != nil {
				addedTracks = append(addedTracks, mt)
			} else {
				p.params.Logger.Warnw("could not find migrated muted track", nil, "cid", cid)
			}
		}
	}
	p.pendingTracksLock.Unlock()

	for _, t := range addedTracks {
		p.handleTrackPublished(t)
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

	p.sendTrackPublished(req.Cid, ti)
}

func (p *ParticipantImpl) SetMigrateInfo(
	previousOffer, previousAnswer *webrtc.SessionDescription,
	mediaTracks []*livekit.TrackPublishedResponse,
	dataChannels []*livekit.DataChannelInfo,
) {
	p.pendingTracksLock.Lock()
	for _, t := range mediaTracks {
		ti := t.GetTrack()

		p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
		p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)

		p.pendingTracks[t.GetCid()] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}, migrated: true}
	}
	p.pendingTracksLock.Unlock()

	p.TransportManager.SetMigrateInfo(previousOffer, previousAnswer, dataChannels)
}

func (p *ParticipantImpl) Start() {
	p.once.Do(func() {
		p.UpTrackManager.Start()
	})
}

func (p *ParticipantImpl) Close(sendLeave bool, reason types.ParticipantCloseReason) error {
	p.params.Logger.Infow("participant closing", "sendLeave", sendLeave, "reason", reason.String())
	if p.isClosed.Swap(true) {
		// already closed
		return nil
	}

	p.clearDisconnectTimer()
	p.clearMigrationTimer()

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

	p.supervisor.Stop()

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
	downTracksToClose := make([]*sfu.DownTrack, 0, len(p.subscribedTracks))
	for _, st := range p.subscribedTracks {
		downTracksToClose = append(downTracksToClose, st.DownTrack())
	}
	p.lock.Unlock()

	p.updateState(livekit.ParticipantInfo_DISCONNECTED)

	// ensure this is synchronized
	p.CloseSignalConnection()
	p.lock.RLock()
	onClose := p.onClose
	p.lock.RUnlock()
	if onClose != nil {
		onClose(p, disallowedSubscriptions)
	}

	// Close peer connections without blocking participant close. If peer connections are gathering candidates
	// Close will block.
	go func() {
		for _, dt := range downTracksToClose {
			dt.CloseWithFlush(sendLeave)
		}

		p.TransportManager.Close()
	}()
	return nil
}

// Negotiate subscriber SDP with client, if force is true, will cencel pending
// negotiate task and negotiate immediately
func (p *ParticipantImpl) Negotiate(force bool) {
	if p.MigrateState() != types.MigrateStateInit {
		p.TransportManager.NegotiateSubscriber(force)
	}
}

func (p *ParticipantImpl) clearMigrationTimer() {
	p.lock.Lock()
	if p.migrationTimer != nil {
		p.migrationTimer.Stop()
		p.migrationTimer = nil
	}
	p.lock.Unlock()
}

func (p *ParticipantImpl) MaybeStartMigration(force bool, onStart func()) bool {
	if !force && !p.TransportManager.HaveAllTransportEverConnected() {
		return false
	}

	if onStart != nil {
		onStart()
	}

	p.CloseSignalConnection()

	//
	// On subscriber peer connection, remote side will try ICE on both
	// pre- and post-migration ICE candidates as the migrating out
	// peer connection leaves itself open to enable transition of
	// media with as less disruption as possible.
	//
	// But, sometimes clients could delay the migration because of
	// pinging the incorrect ICE candidates. Give the remote some time
	// to try and succeed. If not, close the subscriber peer connection
	// and help the remote side to narrow down its ICE candidate pool.
	//
	p.clearMigrationTimer()

	p.lock.Lock()
	p.migrationTimer = time.AfterFunc(migrationWaitDuration, func() {
		p.clearMigrationTimer()

		if p.isClosed.Load() || p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		// TODO: change to debug once we are confident
		p.params.Logger.Infow("closing subscriber peer connection to aid migration")

		//
		// Close all down tracks before closing subscriber peer connection.
		// Closing subscriber peer connection will call `Unbind` on all down tracks.
		// DownTrack close has checks to handle the case of closing before bind.
		// So, an `Unbind` before close would bypass that logic.
		//
		p.lock.Lock()
		downTracksToClose := make([]*sfu.DownTrack, 0, len(p.subscribedTracks))
		for _, st := range p.subscribedTracks {
			downTracksToClose = append(downTracksToClose, st.DownTrack())
		}
		p.lock.Unlock()

		for _, dt := range downTracksToClose {
			dt.CloseWithFlush(false)
		}

		p.TransportManager.SubscriberClose()
	})
	p.lock.Unlock()

	return true
}

func (p *ParticipantImpl) SetMigrateState(s types.MigrateState) {
	preState := p.MigrateState()
	if preState == types.MigrateStateComplete || preState == s {
		return
	}

	p.params.Logger.Debugw("SetMigrateState", "state", s)
	processPendingOffer := false
	p.migrateState.Store(s)
	if s == types.MigrateStateSync {
		processPendingOffer = true
	}

	if s == types.MigrateStateComplete {
		p.TransportManager.ProcessPendingPublisherDataChannels()
	}

	if processPendingOffer {
		p.TransportManager.ProcessPendingPublisherOffer()
	}
}

func (p *ParticipantImpl) MigrateState() types.MigrateState {
	return p.migrateState.Load().(types.MigrateState)
}

// ICERestart restarts subscriber ICE connections
func (p *ParticipantImpl) ICERestart(iceConfig *types.IceConfig) {
	p.clearDisconnectTimer()
	p.clearMigrationTimer()

	for _, t := range p.GetPublishedTracks() {
		t.(types.LocalMediaTrack).Restart()
	}

	p.TransportManager.ICERestart(iceConfig)
}

func (p *ParticipantImpl) OnICEConfigChanged(f func(participant types.LocalParticipant, iceConfig types.IceConfig)) {
	p.lock.Lock()
	p.onICEConfigChanged = f
	p.lock.Unlock()
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

	if avgScore < 4.5 {
		p.params.Logger.Infow(
			"low connection quality score",
			"avgScore", avgScore,
			"publisherScores", publisherScores,
			"subscriberScores", subscriberScores,
		)
	}

	rating := connectionquality.Score2Rating(avgScore)

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

func (p *ParticipantImpl) UpdateSubscribedTrackSettings(trackID livekit.TrackID, settings *livekit.UpdateTrackSettings) error {
	p.lock.Lock()
	p.subscribedTracksSettings[trackID] = settings

	subTrack := p.subscribedTracks[trackID]
	if subTrack == nil {
		// will get set when subscribed track is added
		p.lock.Unlock()
		p.params.Logger.Infow("could not find subscribed track", "trackID", trackID)
		return nil
	}
	p.lock.Unlock()

	subTrack.UpdateSubscriberSettings(settings)
	return nil
}

func (p *ParticipantImpl) VerifySubscribeParticipantInfo(pID livekit.ParticipantID, version uint32) {
	if v, ok := p.updateCache.Get(pID); ok && v.(uint32) >= version {
		return
	}

	if f := p.params.GetParticipantInfo; f != nil {
		if info := f(pID); info != nil {
			p.SendParticipantUpdate([]*livekit.ParticipantInfo{info})
		}
	}
}

// AddSubscribedTrack adds a track to the participant's subscribed list
func (p *ParticipantImpl) AddSubscribedTrack(subTrack types.SubscribedTrack) {
	p.lock.Lock()
	if v, ok := p.trackPublisherVersion[subTrack.ID()]; ok && v > subTrack.PublisherVersion() {
		p.lock.Unlock()
		p.params.Logger.Infow("ignoring add subscribedTrack from older version",
			"current", v,
			"requesting", subTrack.PublisherVersion(),
			"trackID", subTrack.ID(),
		)
		return
	}
	p.params.Logger.Infow("added subscribedTrack",
		"publisherID", subTrack.PublisherID(),
		"publisherIdentity", subTrack.PublisherIdentity(),
		"trackID", subTrack.ID())
	p.trackPublisherVersion[subTrack.ID()] = subTrack.PublisherVersion()

	onSubscribedTo := p.onSubscribedTo

	p.subscribedTracks[subTrack.ID()] = subTrack
	p.supervisor.SetSubscribedTrack(subTrack.ID(), subTrack)

	settings := p.subscribedTracksSettings[subTrack.ID()]
	p.lock.Unlock()

	subTrack.OnBind(func() {
		if p.TransportManager.HasSubscriberEverConnected() {
			subTrack.DownTrack().SetConnected()
		}
		p.TransportManager.AddSubscribedTrack(subTrack)
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
	p.lock.Lock()
	if v, ok := p.trackPublisherVersion[subTrack.ID()]; ok && v > subTrack.PublisherVersion() {
		p.lock.Unlock()
		p.params.Logger.Infow("ignoring remove subscribedTrack from older version",
			"current", v,
			"requesting", subTrack.PublisherVersion(),
			"trackID", subTrack.ID(),
		)
		return
	}
	p.params.Logger.Infow("removed subscribedTrack",
		"publisherID", subTrack.PublisherID(),
		"publisherIdentity", subTrack.PublisherIdentity(),
		"trackID", subTrack.ID(), "kind", subTrack.DownTrack().Kind())
	p.trackPublisherVersion[subTrack.ID()] = subTrack.PublisherVersion()

	delete(p.subscribedTracks, subTrack.ID())
	p.supervisor.ClearSubscribedTrack(subTrack.ID(), subTrack)

	// remove from subscribed map
	numRemaining := 0
	for _, st := range p.subscribedTracks {
		if st.PublisherID() == subTrack.PublisherID() {
			numRemaining++
		}
	}

	//
	// NOTE
	// subscribedTracksSettings should not be deleted on removal as it is needed if corresponding publisher migrated
	// LK-TODO: find a way to clean these up
	//

	if numRemaining == 0 {
		delete(p.subscribedTo, subTrack.PublisherID())
	}
	p.lock.Unlock()

	p.TransportManager.RemoveSubscribedTrack(subTrack)

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
	p.TransportManager.UpdateRTT(rtt, true)

	for _, pt := range p.GetPublishedTracks() {
		pt.(types.LocalMediaTrack).SetRTT(rtt)
	}
}

func (p *ParticipantImpl) setupTransportManager() error {
	tm, err := NewTransportManager(TransportManagerParams{
		Identity: p.params.Identity,
		SID:      p.params.SID,
		// primary connection does not change, canSubscribe can change if permission was updated
		// after the participant has joined
		SubscriberAsPrimary:     p.ProtocolVersion().SubscriberAsPrimary() && p.CanSubscribe(),
		Config:                  p.params.Config,
		ProtocolVersion:         p.params.ProtocolVersion,
		Telemetry:               p.params.Telemetry,
		CongestionControlConfig: p.params.CongestionControlConfig,
		EnabledCodecs:           p.params.EnabledCodecs,
		SimTracks:               p.params.SimTracks,
		ClientConf:              p.params.ClientConf,
		ClientInfo:              p.params.ClientInfo,
		Migration:               p.params.Migration,
		AllowTCPFallback:        p.params.AllowTCPFallback,
		TURNSEnabled:            p.params.TURNSEnabled,
		Logger:                  p.params.Logger,
	})
	if err != nil {
		return err
	}

	tm.OnICEConfigChanged(func(iceConfig types.IceConfig) {
		p.lock.Lock()
		onICEConfigChanged := p.onICEConfigChanged

		if p.params.ClientConf == nil {
			p.params.ClientConf = &livekit.ClientConfiguration{}
		}
		if iceConfig.PreferSub == types.PreferTls {
			p.params.ClientConf.ForceRelay = livekit.ClientConfigSetting_ENABLED
		} else {
			p.params.ClientConf.ForceRelay = livekit.ClientConfigSetting_DISABLED
		}
		p.lock.Unlock()

		if onICEConfigChanged != nil {
			onICEConfigChanged(p, iceConfig)
		}
	})

	tm.OnPublisherICECandidate(func(c *webrtc.ICECandidate) error {
		return p.onICECandidate(c, livekit.SignalTarget_PUBLISHER)
	})
	tm.OnPublisherAnswer(p.onPublisherAnswer)
	tm.OnPublisherTrack(p.onMediaTrack)
	tm.OnPublisherInitialConnected(p.onPublisherInitialConnected)

	tm.OnSubscriberOffer(p.onSubscriberOffer)
	tm.OnSubscriberICECandidate(func(c *webrtc.ICECandidate) error {
		return p.onICECandidate(c, livekit.SignalTarget_SUBSCRIBER)
	})
	tm.OnSubscriberInitialConnected(p.onSubscriberInitialConnected)
	tm.OnSubscriberStreamStateChange(p.onStreamStateChange)

	tm.OnPrimaryTransportInitialConnected(p.onPrimaryTransportInitialConnected)
	tm.OnPrimaryTransportFullyEstablished(p.onPrimaryTransportFullyEstablished)
	tm.OnAnyTransportFailed(p.onAnyTransportFailed)
	tm.OnAnyTransportNegotiationFailed(p.onAnyTransportNegotiationFailed)

	tm.OnDataMessage(p.onDataMessage)
	p.TransportManager = tm
	return nil
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
func (p *ParticipantImpl) onSubscriberOffer(offer webrtc.SessionDescription) error {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		// skip when disconnected
		return nil
	}

	p.params.Logger.Infow("sending offer", "transport", livekit.SignalTarget_SUBSCRIBER)
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
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
			"SSRC", track.SSRC(),
			"mime", track.Codec().MimeType)
	} else {
		p.params.Logger.Warnw("webrtc Track published but can't find MediaTrack", nil,
			"kind", track.Kind().String(),
			"webrtcTrackID", track.ID(),
			"rid", track.RID(),
			"SSRC", track.SSRC(),
			"mime", track.Codec().MimeType)
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

func (p *ParticipantImpl) onDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED || !p.CanPublishData() {
		return
	}

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

func (p *ParticipantImpl) onICECandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) error {
	if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return nil
	}

	if target == livekit.SignalTarget_SUBSCRIBER && p.MigrateState() == types.MigrateStateInit {
		return nil
	}

	return p.sendICECandidate(c, target)
}

func (p *ParticipantImpl) onPublisherInitialConnected() {
	p.supervisor.SetPublisherPeerConnectionConnected(true)
	go p.publisherRTCPWorker()
}

func (p *ParticipantImpl) onSubscriberInitialConnected() {
	go p.subscriberRTCPWorker()

	p.setDowntracksConnected()
}

func (p *ParticipantImpl) onPrimaryTransportInitialConnected() {
	if !p.hasPendingMigratedTrack() && p.MigrateState() == types.MigrateStateSync {
		p.SetMigrateState(types.MigrateStateComplete)
	}
}

func (p *ParticipantImpl) onPrimaryTransportFullyEstablished() {
	p.updateState(livekit.ParticipantInfo_ACTIVE)
}

func (p *ParticipantImpl) clearDisconnectTimer() {
	p.lock.Lock()
	if p.disconnectTimer != nil {
		p.disconnectTimer.Stop()
		p.disconnectTimer = nil
	}
	p.lock.Unlock()
}

func (p *ParticipantImpl) setupDisconnectTimer() {
	p.clearDisconnectTimer()

	p.lock.Lock()
	p.disconnectTimer = time.AfterFunc(disconnectCleanupDuration, func() {
		p.clearDisconnectTimer()

		if p.isClosed.Load() || p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		p.params.Logger.Infow("closing disconnected participant")
		_ = p.Close(true, types.ParticipantCloseReasonPeerConnectionDisconnected)
	})
	p.lock.Unlock()
}

func (p *ParticipantImpl) onAnyTransportFailed() {
	// clients support resuming of connections when websocket becomes disconnected
	p.CloseSignalConnection()

	// detect when participant has actually left.
	p.setupDisconnectTimer()
}

// subscriberRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other publishedTracks in the room.
func (p *ParticipantImpl) subscriberRTCPWorker() {
	defer Recover()
	for {
		if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
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
				if err := p.TransportManager.WriteSubscriberRTCP(pkts); err != nil {
					if err == io.EOF || err == io.ErrClosedPipe {
						return
					}
					p.params.Logger.Errorw("could not send down track reports", err)
				}
			}

			pkts = pkts[:0]
			batchSize = 0
		}

		time.Sleep(5 * time.Second)
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

	// send layer info about max subscription changes to telemetry
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

	p.params.Logger.Infow(
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
	p.pendingTracksLock.Lock()
	defer p.pendingTracksLock.Unlock()

	if req.Sid != "" {
		track := p.GetPublishedTrack(livekit.TrackID(req.Sid))
		if track == nil {
			p.params.Logger.Infow("could not find existing track for multi-codec simulcast", "trackID", req.Sid)
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
		DisableRed: req.DisableRed,
		Stereo:     req.Stereo,
	}
	p.setStableTrackID(req.Cid, ti)
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

	if p.getPublishedTrackBySignalCid(req.Cid) != nil || p.getPublishedTrackBySdpCid(req.Cid) != nil || p.pendingTracks[req.Cid] != nil {
		p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
		p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)

		if p.pendingTracks[req.Cid] == nil {
			p.pendingTracks[req.Cid] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}}
		} else {
			p.pendingTracks[req.Cid].trackInfos = append(p.pendingTracks[req.Cid].trackInfos, ti)
		}
		p.params.Logger.Debugw("pending track queued", "trackID", ti.Sid, "track", ti.String(), "request", req.String())
		return nil
	}

	p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
	p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)

	p.pendingTracks[req.Cid] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}}
	p.params.Logger.Debugw("pending track added", "trackID", ti.Sid, "track", ti.String(), "request", req.String())
	return ti
}

func (p *ParticipantImpl) sendTrackPublished(cid string, ti *livekit.TrackInfo) {
	p.params.Logger.Debugw("sending track published", "cid", cid, "trackInfo", ti.String())
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   cid,
				Track: ti,
			},
		},
	})
}

func (p *ParticipantImpl) SetTrackMuted(trackID livekit.TrackID, muted bool, fromAdmin bool) {
	// when request is coming from admin, send message to current participant
	if fromAdmin {
		p.sendTrackMuted(trackID, muted)
	}

	p.setTrackMuted(trackID, muted)
}

func (p *ParticipantImpl) setTrackMuted(trackID livekit.TrackID, muted bool) {
	p.supervisor.SetPublicationMute(trackID, muted)

	track := p.UpTrackManager.SetPublishedTrackMuted(trackID, muted)

	isPending := false
	p.pendingTracksLock.RLock()
	for _, pti := range p.pendingTracks {
		for _, ti := range pti.trackInfos {
			if livekit.TrackID(ti.Sid) == trackID {
				ti.Muted = muted
				isPending = true
			}
		}
	}
	p.pendingTracksLock.RUnlock()

	if !isPending && track == nil {
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

func (p *ParticipantImpl) mediaTrackReceived(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) (*MediaTrack, bool) {
	p.pendingTracksLock.Lock()
	newTrack := false

	p.params.Logger.Debugw("media track received", "trackID", track.ID(), "kind", track.Kind())
	mid := p.TransportManager.GetPublisherMid(rtpReceiver)
	if mid == "" {
		p.params.Logger.Warnw("could not get mid for track", nil, "trackID", track.ID())
		return nil, false
	}

	// use existing media track to handle simulcast
	mt, ok := p.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti := p.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()))
		if ti == nil {
			p.pendingTracksLock.Unlock()
			return nil, false
		}

		ti.MimeType = track.Codec().MimeType
		mt = p.addMediaTrack(signalCid, track.ID(), ti)
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

func (p *ParticipantImpl) addMigrateMutedTrack(cid string, ti *livekit.TrackInfo) *MediaTrack {
	p.params.Logger.Debugw("add migrate muted track", "cid", cid, "track", ti.String())
	rtpReceiver := p.TransportManager.GetPublisherRTPReceiver(ti.Mid)
	if rtpReceiver == nil {
		p.params.Logger.Errorw("could not find receiver for migrated track", nil, "track", ti.Sid)
		return nil
	}

	mt := p.addMediaTrack(cid, cid, ti)

	potentialCodecs := make([]webrtc.RTPCodecParameters, 0, len(ti.Codecs))
	parameters := rtpReceiver.GetParameters()
	for _, c := range ti.Codecs {
		for _, nc := range parameters.Codecs {
			if strings.EqualFold(nc.MimeType, c.MimeType) {
				potentialCodecs = append(potentialCodecs, nc)
				break
			}
		}
	}
	mt.SetPotentialCodecs(potentialCodecs, parameters.HeaderExtensions)

	for _, codec := range ti.Codecs {
		for ssrc, info := range p.params.SimTracks {
			if info.Mid == codec.Mid {
				mt.MediaTrackReceiver.SetLayerSsrc(codec.MimeType, info.Rid, ssrc)
			}
		}
	}
	mt.SetSimulcast(ti.Simulcast)
	mt.SetMuted(true)

	return mt
}

func (p *ParticipantImpl) addMediaTrack(signalCid string, sdpCid string, ti *livekit.TrackInfo) *MediaTrack {
	mt := NewMediaTrack(MediaTrackParams{
		TrackInfo:           proto.Clone(ti).(*livekit.TrackInfo),
		SignalCid:           signalCid,
		SdpCid:              sdpCid,
		ParticipantID:       p.params.SID,
		ParticipantIdentity: p.params.Identity,
		ParticipantVersion:  p.version.Load(),
		RTCPChan:            p.rtcpCh,
		BufferFactory:       p.params.Config.BufferFactory,
		ReceiverConfig:      p.params.Config.Receiver,
		AudioConfig:         p.params.AudioConfig,
		VideoConfig:         p.params.VideoConfig,
		Telemetry:           p.params.Telemetry,
		Logger:              LoggerWithTrack(p.params.Logger, livekit.TrackID(ti.Sid), false),
		SubscriberConfig:    p.params.Config.Subscriber,
		PLIThrottleConfig:   p.params.PLIThrottleConfig,
		SimTracks:           p.params.SimTracks,
	})

	mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)

	// add to published and clean up pending
	p.supervisor.SetPublishedTrack(livekit.TrackID(ti.Sid), mt)
	p.UpTrackManager.AddPublishedTrack(mt)

	p.pendingTracks[signalCid].trackInfos = p.pendingTracks[signalCid].trackInfos[1:]
	if len(p.pendingTracks[signalCid].trackInfos) == 0 {
		delete(p.pendingTracks, signalCid)
	}

	mt.AddOnClose(func() {
		p.supervisor.ClearPublishedTrack(livekit.TrackID(ti.Sid), mt)

		// re-use track sid
		p.pendingTracksLock.Lock()
		if pti := p.pendingTracks[signalCid]; pti != nil {
			p.sendTrackPublished(signalCid, pti.trackInfos[0])
		} else {
			p.unpublishedTracks = append(p.unpublishedTracks, ti)
		}
		p.pendingTracksLock.Unlock()
	})

	return mt
}

func (p *ParticipantImpl) handleTrackPublished(track types.MediaTrack) {
	if !p.hasPendingMigratedTrack() {
		p.SetMigrateState(types.MigrateStateComplete)
	}

	p.lock.RLock()
	onTrackPublished := p.onTrackPublished
	p.lock.RUnlock()
	if onTrackPublished != nil {
		onTrackPublished(p, track)
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
	pendingInfo := p.pendingTracks[clientId]
	if pendingInfo == nil {
	track_loop:
		for cid, pti := range p.pendingTracks {
			if cid == clientId {
				pendingInfo = pti
				signalCid = cid
				break
			}

			ti := pti.trackInfos[0]
			for _, c := range ti.Codecs {
				if c.Cid == clientId {
					pendingInfo = pti
					signalCid = cid
					break track_loop
				}
			}
		}

		if pendingInfo == nil {
			//
			// If no match on client id, find first one matching type
			// as MediaStreamTrack can change client id when transceiver
			// is added to peer connection.
			//
			for cid, pti := range p.pendingTracks {
				ti := pti.trackInfos[0]
				if ti.Type == kind {
					pendingInfo = pti
					signalCid = cid
					break
				}
			}
		}
	}

	// if still not found, we are done
	if pendingInfo == nil {
		p.params.Logger.Errorw("track info not published prior to track", nil, "clientId", clientId)
		return signalCid, nil
	}

	return signalCid, pendingInfo.trackInfos[0]
}

// setStableTrackID either generates a new TrackID or reuses a previously used one
// for
func (p *ParticipantImpl) setStableTrackID(cid string, info *livekit.TrackInfo) {
	var trackID string
	// if already pending, use the same SID
	// should not happen as this means multiple `AddTrack` requests have been called, but check anyway
	if pti := p.pendingTracks[cid]; pti != nil {
		trackID = pti.trackInfos[0].Sid
	}

	// check against published tracks as re-publish could be happening
	if trackID == "" {
		if pt := p.getPublishedTrackBySignalCid(cid); pt != nil {
			ti := pt.ToProto()
			if ti.Type == info.Type && ti.Source == info.Source && ti.Name == info.Name {
				trackID = ti.Sid
			}
		}
	}

	if trackID == "" {
		// check a previously published matching track
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

func (p *ParticipantImpl) publisherRTCPWorker() {
	defer Recover()

	// read from rtcpChan
	for pkts := range p.rtcpCh {
		if pkts == nil {
			p.params.Logger.Infow("exiting publisher RTCP worker")
			return
		}

		if err := p.TransportManager.WritePublisherRTCP(pkts); err != nil {
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
	for clientID, pti := range p.pendingTracks {
		var trackInfos []string
		for _, ti := range pti.trackInfos {
			trackInfos = append(trackInfos, ti.String())
		}

		pendingTrackInfo[clientID] = map[string]interface{}{
			"TrackInfos": trackInfos,
			"Migrated":   pti.migrated,
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

func (p *ParticipantImpl) GetSubscribedTracks() []types.SubscribedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()

	tracks := make([]types.SubscribedTrack, 0, len(p.subscribedTracks))
	for _, t := range p.subscribedTracks {
		tracks = append(tracks, t)
	}
	return tracks
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

func (p *ParticipantImpl) CacheDownTrack(trackID livekit.TrackID, rtpTransceiver *webrtc.RTPTransceiver, downTrack sfu.DownTrackState) {
	p.lock.Lock()
	if existing := p.cachedDownTracks[trackID]; existing != nil && existing.transceiver != rtpTransceiver {
		p.params.Logger.Infow("cached transceiver changed", "trackID", trackID)
	}
	p.cachedDownTracks[trackID] = &downTrackState{transceiver: rtpTransceiver, downTrack: downTrack}
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

func (p *ParticipantImpl) GetCachedDownTrack(trackID livekit.TrackID) (*webrtc.RTPTransceiver, sfu.DownTrackState) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	dts := p.cachedDownTracks[trackID]
	if dts != nil {
		return dts.transceiver, dts.downTrack
	}

	return nil, sfu.DownTrackState{}
}

func (p *ParticipantImpl) onAnyTransportNegotiationFailed() {
	p.params.Logger.Infow("negotiation failed, starting full reconnect")
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: &livekit.LeaveRequest{
				CanReconnect: true,
				Reason:       types.ParticipantCloseReasonNegotiateFailed.ToDisconnectReason(),
			},
		},
	})
	p.CloseSignalConnection()

	// on a full reconnect, no need to supervise this participant anymore
	p.supervisor.Stop()
}

func (p *ParticipantImpl) EnqueueSubscribeTrack(trackID livekit.TrackID, isRelayed bool, f func(sub types.LocalParticipant) error) {
	p.params.Logger.Debugw("queuing subscribe", "trackID", trackID, "relayed", isRelayed)

	p.supervisor.UpdateSubscription(trackID, true)

	p.lock.Lock()
	p.subscriptionRequestsQueue[trackID] = append(p.subscriptionRequestsQueue[trackID], SubscribeRequest{
		requestType: SubscribeRequestTypeAdd,
		addCb:       f,
	})
	p.lock.Unlock()

	go p.ProcessSubscriptionRequestsQueue(trackID)
}

func (p *ParticipantImpl) EnqueueUnsubscribeTrack(trackID livekit.TrackID, isRelayed bool, willBeResumed bool, f func(subscriberID livekit.ParticipantID, willBeResumed bool) error) {
	p.params.Logger.Debugw("queuing unsubscribe", "trackID", trackID, "relayed", isRelayed)

	p.supervisor.UpdateSubscription(trackID, false)

	p.lock.Lock()
	p.subscriptionRequestsQueue[trackID] = append(p.subscriptionRequestsQueue[trackID], SubscribeRequest{
		requestType:   SubscribeRequestTypeRemove,
		willBeResumed: willBeResumed,
		removeCb:      f,
	})
	p.lock.Unlock()

	go p.ProcessSubscriptionRequestsQueue(trackID)
}

func (p *ParticipantImpl) ProcessSubscriptionRequestsQueue(trackID livekit.TrackID) {
	p.lock.Lock()
	if p.subscriptionInProgress[trackID] || len(p.subscriptionRequestsQueue[trackID]) == 0 {
		p.lock.Unlock()
		return
	}

	request := p.subscriptionRequestsQueue[trackID][0]
	p.subscriptionRequestsQueue[trackID] = p.subscriptionRequestsQueue[trackID][1:]
	if len(p.subscriptionRequestsQueue[trackID]) == 0 {
		delete(p.subscriptionRequestsQueue, trackID)
	}

	p.subscriptionInProgress[trackID] = true
	p.lock.Unlock()

	switch request.requestType {
	case SubscribeRequestTypeAdd:
		err := request.addCb(p)
		if err != nil {
			if err != errAlreadySubscribed {
				p.params.Logger.Errorw("error adding subscriber", err, "trackID", trackID)
			}

			// process pending request even if adding errors out
			p.ClearInProgressAndProcessSubscriptionRequestsQueue(trackID)
		}

	case SubscribeRequestTypeRemove:
		err := request.removeCb(p.ID(), request.willBeResumed)
		if err != nil {
			p.ClearInProgressAndProcessSubscriptionRequestsQueue(trackID)
		}

	default:
		p.params.Logger.Warnw("unknown request type", nil,
			"requestType", request.requestType)

		// let the queue move forward
		p.ClearInProgressAndProcessSubscriptionRequestsQueue(trackID)
	}
}

func (p *ParticipantImpl) ClearInProgressAndProcessSubscriptionRequestsQueue(trackID livekit.TrackID) {
	p.lock.Lock()
	delete(p.subscriptionInProgress, trackID)
	p.lock.Unlock()

	go p.ProcessSubscriptionRequestsQueue(trackID)
}

func (p *ParticipantImpl) UpdateSubscribedQuality(nodeID livekit.NodeID, trackID livekit.TrackID, maxQualities []types.SubscribedCodecQuality) error {
	track := p.GetPublishedTrack(trackID)
	if track == nil {
		p.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.(types.LocalMediaTrack).NotifySubscriberNodeMaxQuality(nodeID, maxQualities)
	return nil
}

func (p *ParticipantImpl) UpdateMediaLoss(nodeID livekit.NodeID, trackID livekit.TrackID, fractionalLoss uint32) error {
	track := p.GetPublishedTrack(trackID)
	if track == nil {
		p.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.(types.LocalMediaTrack).NotifySubscriberNodeMediaLoss(nodeID, uint8(fractionalLoss))
	return nil
}

func codecsFromMediaDescription(m *sdp.MediaDescription) (out []sdp.Codec, err error) {
	s := &sdp.SessionDescription{
		MediaDescriptions: []*sdp.MediaDescription{m},
	}

	for _, payloadStr := range m.MediaName.Formats {
		payloadType, err := strconv.ParseUint(payloadStr, 10, 8)
		if err != nil {
			return nil, err
		}

		codec, err := s.GetCodecForPayloadType(uint8(payloadType))
		if err != nil {
			if payloadType == 0 {
				continue
			}
			return nil, err
		}

		out = append(out, codec)
	}

	return out, nil
}
