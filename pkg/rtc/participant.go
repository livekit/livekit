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
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/metric"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/supervisor"
	"github.com/livekit/livekit-server/pkg/rtc/transport"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/sfu/streamallocator"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	sutils "github.com/livekit/livekit-server/pkg/utils"
)

const (
	sdBatchSize       = 30
	rttUpdateInterval = 5 * time.Second

	disconnectCleanupDuration = 5 * time.Second
	migrationWaitDuration     = 3 * time.Second

	PingIntervalSeconds = 5
	PingTimeoutSeconds  = 15
)

type pendingTrackInfo struct {
	trackInfos []*livekit.TrackInfo
	migrated   bool
	createdAt  time.Time

	// indicates if this track is queued for publishing to avoid a track has been published
	// before the previous track is unpublished(closed) because client is allowed to neogtiate
	// webrtc track before AddTrackRequest return to speed up the publishing process
	queued bool
}

type pendingRemoteTrack struct {
	track    *webrtc.TrackRemote
	receiver *webrtc.RTPReceiver
}

type downTrackState struct {
	transceiver *webrtc.RTPTransceiver
	downTrack   sfu.DownTrackState
}

type postRtcpOp struct {
	*ParticipantImpl
	pkts []rtcp.Packet
}

// ---------------------------------------------------------------

type participantUpdateInfo struct {
	identity  livekit.ParticipantIdentity
	version   uint32
	state     livekit.ParticipantInfo_State
	updatedAt time.Time
}

func (p participantUpdateInfo) String() string {
	return fmt.Sprintf("identity: %s, version: %d, state: %s, updatedAt: %s", p.identity, p.version, p.state.String(), p.updatedAt.String())
}

// ---------------------------------------------------------------

type ParticipantParams struct {
	Identity                livekit.ParticipantIdentity
	Name                    livekit.ParticipantName
	SID                     livekit.ParticipantID
	Config                  *WebRTCConfig
	Sink                    routing.MessageSink
	AudioConfig             sfu.AudioConfig
	VideoConfig             config.VideoConfig
	LimitConfig             config.LimitConfig
	ProtocolVersion         types.ProtocolVersion
	SessionStartTime        time.Time
	Telemetry               telemetry.TelemetryService
	Trailer                 []byte
	PLIThrottleConfig       sfu.PLIThrottleConfig
	CongestionControlConfig config.CongestionControlConfig
	// codecs that are enabled for this room
	PublishEnabledCodecs           []*livekit.Codec
	SubscribeEnabledCodecs         []*livekit.Codec
	Logger                         logger.Logger
	SimTracks                      map[uint32]SimulcastTrackInfo
	Grants                         *auth.ClaimGrants
	InitialVersion                 uint32
	ClientConf                     *livekit.ClientConfiguration
	ClientInfo                     ClientInfo
	Region                         string
	Migration                      bool
	Reconnect                      bool
	AdaptiveStream                 bool
	AllowTCPFallback               bool
	TCPFallbackRTTThreshold        int
	AllowUDPUnstableFallback       bool
	TURNSEnabled                   bool
	GetParticipantInfo             func(pID livekit.ParticipantID) *livekit.ParticipantInfo
	GetRegionSettings              func(ip string) *livekit.RegionSettings
	GetSubscriberForwarderState    func(p types.LocalParticipant) (map[livekit.TrackID]*livekit.RTPForwarderState, error)
	DisableSupervisor              bool
	ReconnectOnPublicationError    bool
	ReconnectOnSubscriptionError   bool
	ReconnectOnDataChannelError    bool
	DataChannelMaxBufferedAmount   uint64
	VersionGenerator               utils.TimedVersionGenerator
	TrackResolver                  types.MediaTrackResolver
	DisableDynacast                bool
	SubscriberAllowPause           bool
	SubscriptionLimitAudio         int32
	SubscriptionLimitVideo         int32
	PlayoutDelay                   *livekit.PlayoutDelay
	SyncStreams                    bool
	ForwardStats                   *sfu.ForwardStats
	DisableSenderReportPassThrough bool
	MetricConfig                   metric.MetricConfig
	UseSendSideBWEInterceptor      bool
	UseSendSideBWE                 bool
	UseOneShotSignallingMode       bool
}

type ParticipantImpl struct {
	// utils.TimedVersion is a atomic. To be correctly aligned also on 32bit archs
	// 64it atomics need to be at the front of a struct
	timedVersion utils.TimedVersion

	params ParticipantParams

	isClosed    atomic.Bool
	closeReason atomic.Value // types.ParticipantCloseReason

	state        atomic.Value // livekit.ParticipantInfo_State
	disconnected chan struct{}

	resSinkMu sync.Mutex
	resSink   routing.MessageSink

	grants      atomic.Pointer[auth.ClaimGrants]
	isPublisher atomic.Bool

	sessionStartRecorded atomic.Bool
	lastActiveAt         atomic.Pointer[time.Time]
	// when first connected
	connectedAt time.Time
	// timer that's set when disconnect is detected on primary PC
	disconnectTimer *time.Timer
	migrationTimer  *time.Timer

	pubRTCPQueue *sutils.TypedOpsQueue[postRtcpOp]

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// client intended to publish, yet to be reconciled
	pendingTracksLock       utils.RWMutex
	pendingTracks           map[string]*pendingTrackInfo
	pendingPublishingTracks map[livekit.TrackID]*pendingTrackInfo
	pendingRemoteTracks     []*pendingRemoteTrack

	// supported codecs
	enabledPublishCodecs   []*livekit.Codec
	enabledSubscribeCodecs []*livekit.Codec

	*TransportManager
	*UpTrackManager
	*SubscriptionManager

	icQueue [2]atomic.Pointer[webrtc.ICECandidate]

	requireBroadcast bool
	// queued participant updates before join response is sent
	// guarded by updateLock
	queuedUpdates []*livekit.ParticipantInfo
	// cache of recently sent updates, to ensuring ordering by version
	// guarded by updateLock
	updateCache *lru.Cache[livekit.ParticipantID, participantUpdateInfo]
	updateLock  utils.Mutex

	dataChannelStats *telemetry.BytesTrackStats

	rttUpdatedAt time.Time
	lastRTT      uint32

	lock utils.RWMutex

	dirty   atomic.Bool
	version atomic.Uint32

	// callbacks & handlers
	onTrackPublished     func(types.LocalParticipant, types.MediaTrack)
	onTrackUpdated       func(types.LocalParticipant, types.MediaTrack)
	onTrackUnpublished   func(types.LocalParticipant, types.MediaTrack)
	onStateChange        func(p types.LocalParticipant, state livekit.ParticipantInfo_State)
	onMigrateStateChange func(p types.LocalParticipant, migrateState types.MigrateState)
	onParticipantUpdate  func(types.LocalParticipant)
	onDataPacket         func(types.LocalParticipant, livekit.DataPacket_Kind, *livekit.DataPacket)
	onMetrics            func(types.LocalParticipant, *livekit.DataPacket)

	migrateState atomic.Value // types.MigrateState

	onClose            func(types.LocalParticipant)
	onClaimsChanged    func(participant types.LocalParticipant)
	onICEConfigChanged func(participant types.LocalParticipant, iceConfig *livekit.ICEConfig)

	cachedDownTracks map[livekit.TrackID]*downTrackState
	forwarderState   map[livekit.TrackID]*livekit.RTPForwarderState

	supervisor *supervisor.ParticipantSupervisor

	tracksQuality map[livekit.TrackID]livekit.ConnectionQuality

	metricTimestamper *metric.MetricTimestamper

	// reliable data packets received while transitioning from JOINED to ACTIVE state. Stored for later delivery
	reliableDataPacketsQueue []*livekit.DataPacket

	// loggers for publisher and subscriber
	pubLogger logger.Logger
	subLogger logger.Logger
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
		params:       params,
		disconnected: make(chan struct{}),
		pubRTCPQueue: sutils.NewTypedOpsQueue[postRtcpOp](sutils.OpsQueueParams{
			Name:    "pub-rtcp",
			MinSize: 64,
			Logger:  params.Logger,
		}),
		pendingTracks:           make(map[string]*pendingTrackInfo),
		pendingPublishingTracks: make(map[livekit.TrackID]*pendingTrackInfo),
		connectedAt:             time.Now(),
		rttUpdatedAt:            time.Now(),
		cachedDownTracks:        make(map[livekit.TrackID]*downTrackState),
		dataChannelStats: telemetry.NewBytesTrackStats(
			telemetry.BytesTrackIDForParticipantID(telemetry.BytesTrackTypeData, params.SID),
			params.SID,
			params.Telemetry,
		),
		tracksQuality:            make(map[livekit.TrackID]livekit.ConnectionQuality),
		reliableDataPacketsQueue: make([]*livekit.DataPacket, 0),
		metricTimestamper: metric.NewMetricTimestamper(metric.MetricTimestamperParams{
			Config: params.MetricConfig.Timestamper,
			Logger: params.Logger,
		}),
		pubLogger: params.Logger.WithComponent(sutils.ComponentPub),
		subLogger: params.Logger.WithComponent(sutils.ComponentSub),
	}
	if !params.DisableSupervisor {
		p.supervisor = supervisor.NewParticipantSupervisor(supervisor.ParticipantSupervisorParams{Logger: params.Logger})
	}
	p.closeReason.Store(types.ParticipantCloseReasonNone)
	p.version.Store(params.InitialVersion)
	p.timedVersion.Update(params.VersionGenerator.Next())
	p.migrateState.Store(types.MigrateStateInit)
	p.state.Store(livekit.ParticipantInfo_JOINING)
	p.grants.Store(params.Grants.Clone())
	p.SetResponseSink(params.Sink)
	p.setupEnabledCodecs(params.PublishEnabledCodecs, params.SubscribeEnabledCodecs, params.ClientConf.GetDisabledCodecs())

	if p.supervisor != nil {
		p.supervisor.OnPublicationError(p.onPublicationError)
	}

	var err error
	// keep last participants and when updates were sent
	if p.updateCache, err = lru.New[livekit.ParticipantID, participantUpdateInfo](128); err != nil {
		return nil, err
	}

	err = p.setupTransportManager()
	if err != nil {
		return nil, err
	}

	p.setupUpTrackManager()
	p.setupSubscriptionManager()

	return p, nil
}

func (p *ParticipantImpl) GetTrailer() []byte {
	trailer := make([]byte, len(p.params.Trailer))
	copy(trailer, p.params.Trailer)
	return trailer
}

func (p *ParticipantImpl) GetLogger() logger.Logger {
	return p.params.Logger
}

func (p *ParticipantImpl) GetAdaptiveStream() bool {
	return p.params.AdaptiveStream
}

func (p *ParticipantImpl) GetPacer() pacer.Pacer {
	return p.TransportManager.GetSubscriberPacer()
}

func (p *ParticipantImpl) GetDisableSenderReportPassThrough() bool {
	return p.params.DisableSenderReportPassThrough
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

func (p *ParticipantImpl) Kind() livekit.ParticipantInfo_Kind {
	return p.grants.Load().GetParticipantKind()
}

func (p *ParticipantImpl) IsRecorder() bool {
	grants := p.grants.Load()
	return grants.GetParticipantKind() == livekit.ParticipantInfo_EGRESS || grants.Video.Recorder
}

func (p *ParticipantImpl) IsAgent() bool {
	grants := p.grants.Load()
	return grants.GetParticipantKind() == livekit.ParticipantInfo_AGENT || grants.Video.Agent
}

func (p *ParticipantImpl) IsDependent() bool {
	grants := p.grants.Load()
	switch grants.GetParticipantKind() {
	case livekit.ParticipantInfo_AGENT, livekit.ParticipantInfo_EGRESS:
		return true
	default:
		return grants.Video.Agent || grants.Video.Recorder
	}
}

func (p *ParticipantImpl) ProtocolVersion() types.ProtocolVersion {
	return p.params.ProtocolVersion
}

func (p *ParticipantImpl) IsReady() bool {
	state := p.State()

	// when migrating, there is no JoinResponse, state transitions from JOINING -> ACTIVE -> DISCONNECTED
	// so JOINING is considered ready.
	if p.params.Migration {
		return state != livekit.ParticipantInfo_DISCONNECTED
	}

	// when not migrating, there is a JoinResponse, state transitions from JOINING -> JOINED -> ACTIVE -> DISCONNECTED
	return state == livekit.ParticipantInfo_JOINED || state == livekit.ParticipantInfo_ACTIVE
}

func (p *ParticipantImpl) IsDisconnected() bool {
	return p.State() == livekit.ParticipantInfo_DISCONNECTED
}

func (p *ParticipantImpl) Disconnected() <-chan struct{} {
	return p.disconnected
}

func (p *ParticipantImpl) IsIdle() bool {
	// check if there are any published tracks that are subscribed
	for _, t := range p.GetPublishedTracks() {
		if t.GetNumSubscribers() > 0 {
			return false
		}
	}

	return !p.SubscriptionManager.HasSubscriptions()
}

func (p *ParticipantImpl) ConnectedAt() time.Time {
	return p.connectedAt
}

func (p *ParticipantImpl) GetClientInfo() *livekit.ClientInfo {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.params.ClientInfo.ClientInfo
}

func (p *ParticipantImpl) GetClientConfiguration() *livekit.ClientConfiguration {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.params.ClientConf
}

func (p *ParticipantImpl) GetBufferFactory() *buffer.Factory {
	return p.params.Config.BufferFactory
}

// CheckMetadataLimits check if name/metadata/attributes of a participant is within configured limits
func (p *ParticipantImpl) CheckMetadataLimits(
	name string,
	metadata string,
	attributes map[string]string,
) error {
	if !p.params.LimitConfig.CheckParticipantNameLength(name) {
		return ErrNameExceedsLimits
	}

	if !p.params.LimitConfig.CheckMetadataSize(metadata) {
		return ErrMetadataExceedsLimits
	}

	if !p.params.LimitConfig.CheckAttributesSize(attributes) {
		return ErrAttributesExceedsLimits
	}

	return nil
}

// SetName attaches name to the participant
func (p *ParticipantImpl) SetName(name string) {
	p.lock.Lock()
	grants := p.grants.Load()
	if grants.Name == name {
		p.lock.Unlock()
		return
	}

	grants = grants.Clone()
	grants.Name = name
	p.grants.Store(grants)
	p.dirty.Store(true)

	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged
	p.lock.Unlock()

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
}

// SetMetadata attaches metadata to the participant
func (p *ParticipantImpl) SetMetadata(metadata string) {
	p.lock.Lock()
	grants := p.grants.Load()
	if grants.Metadata == metadata {
		p.lock.Unlock()
		return
	}

	grants = grants.Clone()
	grants.Metadata = metadata
	p.grants.Store(grants)
	p.requireBroadcast = p.requireBroadcast || metadata != ""
	p.dirty.Store(true)

	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged
	p.lock.Unlock()

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
}

func (p *ParticipantImpl) SetAttributes(attrs map[string]string) {
	if len(attrs) == 0 {
		return
	}
	p.lock.Lock()
	grants := p.grants.Load().Clone()
	if grants.Attributes == nil {
		grants.Attributes = make(map[string]string)
	}
	var keysToDelete []string
	for k, v := range attrs {
		if v == "" {
			keysToDelete = append(keysToDelete, k)
		} else {
			grants.Attributes[k] = v
		}
	}
	for _, k := range keysToDelete {
		delete(grants.Attributes, k)
	}

	p.grants.Store(grants)
	p.requireBroadcast = true // already checked above
	p.dirty.Store(true)

	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged
	p.lock.Unlock()

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
}

func (p *ParticipantImpl) ClaimGrants() *auth.ClaimGrants {
	return p.grants.Load()
}

func (p *ParticipantImpl) SetPermission(permission *livekit.ParticipantPermission) bool {
	if permission == nil {
		return false
	}
	p.lock.Lock()
	grants := p.grants.Load()

	if grants.Video.MatchesPermission(permission) {
		p.lock.Unlock()
		return false
	}

	p.params.Logger.Infow("updating participant permission", "permission", permission)

	grants = grants.Clone()
	grants.Video.UpdateFromPermission(permission)
	p.grants.Store(grants)
	p.dirty.Store(true)

	canPublish := grants.Video.GetCanPublish()
	canSubscribe := grants.Video.GetCanSubscribe()

	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged

	isPublisher := canPublish && p.TransportManager.IsPublisherEstablished()
	p.requireBroadcast = p.requireBroadcast || isPublisher
	p.lock.Unlock()

	// publish permission has been revoked then remove offending tracks
	for _, track := range p.GetPublishedTracks() {
		if !grants.Video.GetCanPublishSource(track.Source()) {
			p.removePublishedTrack(track)
		}
	}

	if canSubscribe {
		// reconcile everything
		p.SubscriptionManager.ReconcileAll()
	} else {
		// revoke all subscriptions
		for _, st := range p.SubscriptionManager.GetSubscribedTracks() {
			st.MediaTrack().RemoveSubscriber(p.ID(), false)
		}
	}

	// update isPublisher attribute
	p.isPublisher.Store(isPublisher)

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
	return true
}

func (p *ParticipantImpl) CanSkipBroadcast() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return !p.requireBroadcast
}

func (p *ParticipantImpl) maybeIncVersion() {
	if p.dirty.Load() {
		p.lock.Lock()
		if p.dirty.Swap(false) {
			p.version.Inc()
			p.timedVersion.Update(p.params.VersionGenerator.Next())
		}
		p.lock.Unlock()
	}
}

func (p *ParticipantImpl) Version() utils.TimedVersion {
	p.maybeIncVersion()

	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.timedVersion
}

func (p *ParticipantImpl) ToProtoWithVersion() (*livekit.ParticipantInfo, utils.TimedVersion) {
	p.maybeIncVersion()

	p.lock.RLock()
	grants := p.grants.Load()
	v := p.version.Load()
	piv := p.timedVersion

	pi := &livekit.ParticipantInfo{
		Sid:              string(p.params.SID),
		Identity:         string(p.params.Identity),
		Name:             grants.Name,
		State:            p.State(),
		JoinedAt:         p.ConnectedAt().Unix(),
		Version:          v,
		Permission:       grants.Video.ToPermission(),
		Metadata:         grants.Metadata,
		Attributes:       grants.Attributes,
		Region:           p.params.Region,
		IsPublisher:      p.IsPublisher(),
		Kind:             grants.GetParticipantKind(),
		DisconnectReason: p.CloseReason().ToDisconnectReason(),
	}
	p.lock.RUnlock()

	p.pendingTracksLock.RLock()
	pi.Tracks = p.UpTrackManager.ToProto()

	// add any pending migrating tracks, else an update could delete/unsubscribe from yet to be published, migrating tracks
	maybeAdd := func(pti *pendingTrackInfo) {
		if !pti.migrated {
			return
		}

		found := false
		for _, ti := range pi.Tracks {
			if ti.Sid == pti.trackInfos[0].Sid {
				found = true
				break
			}
		}

		if !found {
			pi.Tracks = append(pi.Tracks, pti.trackInfos[0])
		}
	}

	for _, pt := range p.pendingTracks {
		maybeAdd(pt)
	}
	for _, ppt := range p.pendingPublishingTracks {
		maybeAdd(ppt)
	}
	p.pendingTracksLock.RUnlock()

	return pi, piv
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	pi, _ := p.ToProtoWithVersion()
	return pi
}

// callbacks for clients

func (p *ParticipantImpl) OnTrackPublished(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackPublished = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnTrackPublished() func(types.LocalParticipant, types.MediaTrack) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onTrackPublished
}

func (p *ParticipantImpl) OnTrackUnpublished(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackUnpublished = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnTrackUnpublished() func(types.LocalParticipant, types.MediaTrack) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onTrackUnpublished
}

func (p *ParticipantImpl) OnStateChange(callback func(p types.LocalParticipant, state livekit.ParticipantInfo_State)) {
	p.lock.Lock()
	p.onStateChange = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnStateChange() func(p types.LocalParticipant, state livekit.ParticipantInfo_State) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onStateChange
}

func (p *ParticipantImpl) OnMigrateStateChange(callback func(p types.LocalParticipant, state types.MigrateState)) {
	p.lock.Lock()
	p.onMigrateStateChange = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnMigrateStateChange() func(p types.LocalParticipant, state types.MigrateState) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onMigrateStateChange
}

func (p *ParticipantImpl) OnTrackUpdated(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackUpdated = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnTrackUpdated() func(types.LocalParticipant, types.MediaTrack) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onTrackUpdated
}

func (p *ParticipantImpl) OnParticipantUpdate(callback func(types.LocalParticipant)) {
	p.lock.Lock()
	p.onParticipantUpdate = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnDataPacket(callback func(types.LocalParticipant, livekit.DataPacket_Kind, *livekit.DataPacket)) {
	p.lock.Lock()
	p.onDataPacket = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnDataPacket() func(types.LocalParticipant, livekit.DataPacket_Kind, *livekit.DataPacket) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onDataPacket
}

func (p *ParticipantImpl) OnMetrics(callback func(types.LocalParticipant, *livekit.DataPacket)) {
	p.lock.Lock()
	p.onMetrics = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnMetrics() func(types.LocalParticipant, *livekit.DataPacket) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onMetrics
}

func (p *ParticipantImpl) OnClose(callback func(types.LocalParticipant)) {
	p.lock.Lock()
	p.onClose = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnClaimsChanged(callback func(types.LocalParticipant)) {
	p.lock.Lock()
	p.onClaimsChanged = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) HandleSignalSourceClose() {
	p.TransportManager.SetSignalSourceValid(false)

	if !p.HasConnected() {
		_ = p.Close(false, types.ParticipantCloseReasonSignalSourceClose, false)
	}
}

func (p *ParticipantImpl) synthesizeAddTrackRequests(offer webrtc.SessionDescription) error {
	parsed, err := offer.Unmarshal()
	if err != nil {
		return err
	}

	for _, m := range parsed.MediaDescriptions {
		if !strings.EqualFold(m.MediaName.Media, "audio") {
			// ONE-SHOT-SIGNALLING-MODE-TODO: support video
			continue
		}

		trackID := ""

		msid, ok := m.Attribute(sdp.AttrKeyMsid)
		if ok {
			if split := strings.Split(msid, " "); len(split) == 2 {
				trackID = split[1]
			}
		}

		if trackID == "" {
			attr, ok := m.Attribute(sdp.AttrKeySSRC)
			if ok {
				split := strings.Split(attr, " ")
				if len(split) == 3 && strings.HasPrefix(split[1], "msid:") {
					trackID = split[2]
				}
			}
		}

		if trackID == "" {
			trackID = guid.New(utils.TrackPrefix)
		}

		req := &livekit.AddTrackRequest{
			Cid:        trackID,
			Name:       "synthesized-microphone",
			Source:     livekit.TrackSource_MICROPHONE,
			Type:       livekit.TrackType_AUDIO,
			DisableDtx: true,
			Stereo:     false,
			Stream:     "camera",
		}
		p.AddTrack(req)
	}
	return nil
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(offer webrtc.SessionDescription) error {
	p.pubLogger.Debugw("received offer", "transport", livekit.SignalTarget_PUBLISHER, "offer", offer)

	if p.params.UseOneShotSignallingMode {
		if err := p.synthesizeAddTrackRequests(offer); err != nil {
			return err
		}
	}

	shouldPend := false
	if p.MigrateState() == types.MigrateStateInit {
		shouldPend = true
	}

	offer = p.setCodecPreferencesForPublisher(offer)
	err := p.TransportManager.HandleOffer(offer, shouldPend)
	if p.params.UseOneShotSignallingMode {
		p.updateState(livekit.ParticipantInfo_ACTIVE)
	}
	return err
}

func (p *ParticipantImpl) onPublisherAnswer(answer webrtc.SessionDescription) error {
	if p.IsClosed() || p.IsDisconnected() {
		return nil
	}

	answer = p.configurePublisherAnswer(answer)
	p.pubLogger.Debugw("sending answer", "transport", livekit.SignalTarget_PUBLISHER, "answer", answer)
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})
}

func (p *ParticipantImpl) GetAnswer() (webrtc.SessionDescription, error) {
	if p.IsClosed() || p.IsDisconnected() {
		return webrtc.SessionDescription{}, ErrParticipantSessionClosed
	}

	answer, err := p.TransportManager.GetAnswer()
	if err != nil {
		return answer, err
	}

	answer = p.configurePublisherAnswer(answer)
	p.pubLogger.Debugw("returning answer", "transport", livekit.SignalTarget_PUBLISHER, "answer", answer)
	return answer, nil
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (p *ParticipantImpl) HandleAnswer(answer webrtc.SessionDescription) {
	p.subLogger.Debugw("received answer", "transport", livekit.SignalTarget_SUBSCRIBER, "answer", answer)

	/* from server received join request to client answer
	 * 1. server send join response & offer
	 * ... swap candidates
	 * 2. client send answer
	 */
	signalConnCost := time.Since(p.ConnectedAt()).Milliseconds()
	p.TransportManager.UpdateSignalingRTT(uint32(signalConnCost))

	p.TransportManager.HandleAnswer(answer)
}

func (p *ParticipantImpl) handleMigrateTracks() {
	// muted track won't send rtp packet, so it is required to add mediatrack manually.
	// But, synthesising track publish for unmuted tracks keeps a consistent path.
	// In both cases (muted and unmuted), when publisher sends media packets, OnTrack would register and go from there.
	var addedTracks []*MediaTrack
	p.pendingTracksLock.Lock()
	for cid, pti := range p.pendingTracks {
		if !pti.migrated {
			continue
		}

		if len(pti.trackInfos) > 1 {
			p.pubLogger.Warnw("too many pending migrated tracks", nil, "trackID", pti.trackInfos[0].Sid, "count", len(pti.trackInfos), "cid", cid)
		}

		mt := p.addMigratedTrack(cid, pti.trackInfos[0])
		if mt != nil {
			addedTracks = append(addedTracks, mt)
		} else {
			p.pubLogger.Warnw("could not find migrated track, migration failed", nil, "cid", cid)
			p.pendingTracksLock.Unlock()
			p.IssueFullReconnect(types.ParticipantCloseReasonMigrateCodecMismatch)
			return
		}
	}

	if len(addedTracks) != 0 {
		p.dirty.Store(true)
	}
	p.pendingTracksLock.Unlock()

	// launch callbacks in goroutine since they could block.
	// callbacks handle webhooks as well as db persistence
	go func() {
		for _, t := range addedTracks {
			p.handleTrackPublished(t)
		}
	}()
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (p *ParticipantImpl) AddTrack(req *livekit.AddTrackRequest) {
	if !p.CanPublishSource(req.Source) {
		p.pubLogger.Warnw("no permission to publish track", nil)
		return
	}

	p.pendingTracksLock.Lock()
	ti := p.addPendingTrackLocked(req)
	p.pendingTracksLock.Unlock()
	if ti == nil {
		return
	}

	p.sendTrackPublished(req.Cid, ti)

	p.handlePendingRemoteTracks()
}

func (p *ParticipantImpl) SetMigrateInfo(
	previousOffer, previousAnswer *webrtc.SessionDescription,
	mediaTracks []*livekit.TrackPublishedResponse,
	dataChannels []*livekit.DataChannelInfo,
) {
	p.pendingTracksLock.Lock()
	for _, t := range mediaTracks {
		ti := t.GetTrack()

		if p.supervisor != nil {
			p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
			p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)
		}

		p.pendingTracks[t.GetCid()] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}, migrated: true, createdAt: time.Now()}
		p.pubLogger.Infow("pending track added (migration)", "trackID", ti.Sid, "track", logger.Proto(ti))
	}
	p.pendingTracksLock.Unlock()

	if len(mediaTracks) != 0 {
		p.setIsPublisher(true)
	}

	p.TransportManager.SetMigrateInfo(previousOffer, previousAnswer, dataChannels)
}

func (p *ParticipantImpl) IsReconnect() bool {
	return p.params.Reconnect
}

func (p *ParticipantImpl) Close(sendLeave bool, reason types.ParticipantCloseReason, isExpectedToResume bool) error {
	if p.isClosed.Swap(true) {
		// already closed
		return nil
	}

	p.params.Logger.Infow(
		"participant closing",
		"sendLeave", sendLeave,
		"reason", reason.String(),
		"isExpectedToResume", isExpectedToResume,
	)
	p.closeReason.Store(reason)
	p.clearDisconnectTimer()
	p.clearMigrationTimer()

	if sendLeave {
		p.sendLeaveRequest(
			reason,
			isExpectedToResume,
			false, // isExpectedToReconnect
			false, // sendOnlyIfSupportingLeaveRequestWithAction
		)
	}

	if p.supervisor != nil {
		p.supervisor.Stop()
	}

	p.pendingTracksLock.Lock()
	p.pendingTracks = make(map[string]*pendingTrackInfo)
	p.pendingPublishingTracks = make(map[livekit.TrackID]*pendingTrackInfo)
	p.pendingTracksLock.Unlock()

	p.UpTrackManager.Close(isExpectedToResume)

	p.updateState(livekit.ParticipantInfo_DISCONNECTED)
	close(p.disconnected)

	// ensure this is synchronized
	p.CloseSignalConnection(types.SignallingCloseReasonParticipantClose)
	p.lock.RLock()
	onClose := p.onClose
	p.lock.RUnlock()
	if onClose != nil {
		onClose(p)
	}

	// Close peer connections without blocking participant Close. If peer connections are gathering candidates
	// Close will block.
	go func() {
		p.SubscriptionManager.Close(isExpectedToResume)
		p.TransportManager.Close()
	}()

	p.dataChannelStats.Stop()
	return nil
}

func (p *ParticipantImpl) IsClosed() bool {
	return p.isClosed.Load()
}

func (p *ParticipantImpl) CloseReason() types.ParticipantCloseReason {
	return p.closeReason.Load().(types.ParticipantCloseReason)
}

// Negotiate subscriber SDP with client, if force is true, will cancel pending
// negotiate task and negotiate immediately
func (p *ParticipantImpl) Negotiate(force bool) {
	if p.params.UseOneShotSignallingMode {
		return
	}

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

func (p *ParticipantImpl) setupMigrationTimerLocked() {
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
	p.migrationTimer = time.AfterFunc(migrationWaitDuration, func() {
		p.clearMigrationTimer()

		if p.IsClosed() || p.IsDisconnected() {
			return
		}
		p.subLogger.Debugw("closing subscriber peer connection to aid migration")

		//
		// Close all down tracks before closing subscriber peer connection.
		// Closing subscriber peer connection will call `Unbind` on all down tracks.
		// DownTrack close has checks to handle the case of closing before bind.
		// So, an `Unbind` before close would bypass that logic.
		//
		p.SubscriptionManager.Close(true)

		p.TransportManager.SubscriberClose()
	})
}

func (p *ParticipantImpl) MaybeStartMigration(force bool, onStart func()) bool {
	if p.params.UseOneShotSignallingMode {
		return false
	}

	allTransportConnected := p.TransportManager.HasSubscriberEverConnected()
	if p.IsPublisher() {
		allTransportConnected = allTransportConnected && p.TransportManager.HasPublisherEverConnected()
	}
	if !force && !allTransportConnected {
		return false
	}

	if onStart != nil {
		onStart()
	}

	p.sendLeaveRequest(
		types.ParticipantCloseReasonMigrationRequested,
		true,  // isExpectedToResume
		false, // isExpectedToReconnect
		true,  // sendOnlyIfSupportingLeaveRequestWithAction
	)
	p.CloseSignalConnection(types.SignallingCloseReasonMigration)

	p.clearMigrationTimer()

	p.lock.Lock()
	p.setupMigrationTimerLocked()
	p.lock.Unlock()

	return true
}

func (p *ParticipantImpl) NotifyMigration() {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.migrationTimer != nil {
		// already set up
		return
	}

	p.setupMigrationTimerLocked()
}

func (p *ParticipantImpl) SetMigrateState(s types.MigrateState) {
	preState := p.MigrateState()
	if preState == types.MigrateStateComplete || preState == s {
		return
	}

	p.params.Logger.Debugw("SetMigrateState", "state", s)
	if s == types.MigrateStateComplete {
		p.handleMigrateTracks()
	}
	p.migrateState.Store(s)
	p.dirty.Store(true)

	switch s {
	case types.MigrateStateSync:
		p.TransportManager.ProcessPendingPublisherOffer()

	case types.MigrateStateComplete:
		p.TransportManager.ProcessPendingPublisherDataChannels()
		p.cacheForwarderState()
	}

	if onMigrateStateChange := p.getOnMigrateStateChange(); onMigrateStateChange != nil {
		go onMigrateStateChange(p, s)
	}
}

func (p *ParticipantImpl) MigrateState() types.MigrateState {
	return p.migrateState.Load().(types.MigrateState)
}

// ICERestart restarts subscriber ICE connections
func (p *ParticipantImpl) ICERestart(iceConfig *livekit.ICEConfig) {
	if p.params.UseOneShotSignallingMode {
		return
	}

	p.clearDisconnectTimer()
	p.clearMigrationTimer()

	for _, t := range p.GetPublishedTracks() {
		t.(types.LocalMediaTrack).Restart()
	}

	if err := p.TransportManager.ICERestart(iceConfig); err != nil {
		p.IssueFullReconnect(types.ParticipantCloseReasonNegotiateFailed)
	}
}

func (p *ParticipantImpl) OnICEConfigChanged(f func(participant types.LocalParticipant, iceConfig *livekit.ICEConfig)) {
	p.lock.Lock()
	p.onICEConfigChanged = f
	p.lock.Unlock()
}

func (p *ParticipantImpl) GetConnectionQuality() *livekit.ConnectionQualityInfo {
	numTracks := 0
	minQuality := livekit.ConnectionQuality_EXCELLENT
	minScore := connectionquality.MaxMOS
	numUpDrops := 0
	numDownDrops := 0

	availableTracks := make(map[livekit.TrackID]bool)

	for _, pt := range p.GetPublishedTracks() {
		numTracks++

		score, quality := pt.(types.LocalMediaTrack).GetConnectionScoreAndQuality()
		if utils.IsConnectionQualityLower(minQuality, quality) {
			minQuality = quality
			minScore = score
		} else if quality == minQuality && score < minScore {
			minScore = score
		}

		p.lock.Lock()
		trackID := pt.ID()
		if prevQuality, ok := p.tracksQuality[trackID]; ok {
			if utils.IsConnectionQualityLower(prevQuality, quality) {
				numUpDrops++
			}
		}
		p.tracksQuality[trackID] = quality
		p.lock.Unlock()

		availableTracks[trackID] = true
	}

	subscribedTracks := p.SubscriptionManager.GetSubscribedTracks()
	for _, subTrack := range subscribedTracks {
		numTracks++

		score, quality := subTrack.DownTrack().GetConnectionScoreAndQuality()
		if utils.IsConnectionQualityLower(minQuality, quality) {
			minQuality = quality
			minScore = score
		} else if quality == minQuality && score < minScore {
			minScore = score
		}

		p.lock.Lock()
		trackID := subTrack.ID()
		if prevQuality, ok := p.tracksQuality[trackID]; ok {
			if utils.IsConnectionQualityLower(prevQuality, quality) {
				numDownDrops++
			}
		}
		p.tracksQuality[trackID] = quality
		p.lock.Unlock()

		availableTracks[trackID] = true
	}

	prometheus.RecordQuality(minQuality, minScore, numUpDrops, numDownDrops)

	// remove unavailable tracks from track quality cache
	p.lock.Lock()
	for trackID := range p.tracksQuality {
		if !availableTracks[trackID] {
			delete(p.tracksQuality, trackID)
		}
	}
	p.lock.Unlock()

	if minQuality == livekit.ConnectionQuality_LOST && !p.ProtocolVersion().SupportsConnectionQualityLost() {
		minQuality = livekit.ConnectionQuality_POOR
	}

	return &livekit.ConnectionQualityInfo{
		ParticipantSid: string(p.ID()),
		Quality:        minQuality,
		Score:          minScore,
	}
}

func (p *ParticipantImpl) IsPublisher() bool {
	return p.isPublisher.Load()
}

func (p *ParticipantImpl) CanPublish() bool {
	return p.grants.Load().Video.GetCanPublish()
}

func (p *ParticipantImpl) CanPublishSource(source livekit.TrackSource) bool {
	return p.grants.Load().Video.GetCanPublishSource(source)
}

func (p *ParticipantImpl) CanSubscribe() bool {
	return p.grants.Load().Video.GetCanSubscribe()
}

func (p *ParticipantImpl) CanPublishData() bool {
	return p.grants.Load().Video.GetCanPublishData()
}

func (p *ParticipantImpl) Hidden() bool {
	return p.grants.Load().Video.Hidden
}

func (p *ParticipantImpl) CanSubscribeMetrics() bool {
	return p.grants.Load().Video.GetCanSubscribeMetrics()
}

func (p *ParticipantImpl) Verify() bool {
	state := p.State()
	isActive := state != livekit.ParticipantInfo_JOINING && state != livekit.ParticipantInfo_JOINED
	if p.params.UseOneShotSignallingMode {
		isActive = isActive && p.TransportManager.HasPublisherEverConnected()
	}

	return isActive
}

func (p *ParticipantImpl) VerifySubscribeParticipantInfo(pID livekit.ParticipantID, version uint32) {
	if !p.IsReady() {
		// we have not sent a JoinResponse yet. metadata would be covered in JoinResponse
		return
	}
	if info, ok := p.updateCache.Get(pID); ok && info.version >= version {
		return
	}

	if f := p.params.GetParticipantInfo; f != nil {
		if info := f(pID); info != nil {
			_ = p.SendParticipantUpdate([]*livekit.ParticipantInfo{info})
		}
	}
}

// onTrackSubscribed handles post-processing after a track is subscribed
func (p *ParticipantImpl) onTrackSubscribed(subTrack types.SubscribedTrack) {
	if p.params.ClientInfo.FireTrackByRTPPacket() {
		subTrack.DownTrack().SetActivePaddingOnMuteUpTrack()
	}

	subTrack.AddOnBind(func(err error) {
		if err != nil {
			return
		}
		if p.params.UseOneShotSignallingMode {
			if p.TransportManager.HasPublisherEverConnected() {
				dt := subTrack.DownTrack()
				dt.SeedState(sfu.DownTrackState{ForwarderState: p.getAndDeleteForwarderState(subTrack.ID())})
				dt.SetConnected()
			}
			// ONE-SHOT-SIGNALLING-MODE-TODO: video support should add to publisher PC for congestion control
		} else {
			if p.TransportManager.HasSubscriberEverConnected() {
				dt := subTrack.DownTrack()
				dt.SeedState(sfu.DownTrackState{ForwarderState: p.getAndDeleteForwarderState(subTrack.ID())})
				dt.SetConnected()
			}
			p.TransportManager.AddSubscribedTrack(subTrack)
		}
	})
}

// onTrackUnsubscribed handles post-processing after a track is unsubscribed
func (p *ParticipantImpl) onTrackUnsubscribed(subTrack types.SubscribedTrack) {
	p.TransportManager.RemoveSubscribedTrack(subTrack)
}

func (p *ParticipantImpl) SubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool) {
	p.subLogger.Debugw("sending subscription permission update", "publisherID", publisherID, "trackID", trackID, "allowed", allowed)
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
		p.subLogger.Errorw("could not send subscription permission update", err)
	}
}

func (p *ParticipantImpl) UpdateMediaRTT(rtt uint32) {
	now := time.Now()
	p.lock.Lock()
	if now.Sub(p.rttUpdatedAt) < rttUpdateInterval || p.lastRTT == rtt {
		p.lock.Unlock()
		return
	}
	p.rttUpdatedAt = now
	p.lastRTT = rtt
	p.lock.Unlock()
	p.TransportManager.UpdateMediaRTT(rtt)

	for _, pt := range p.GetPublishedTracks() {
		pt.(types.LocalMediaTrack).SetRTT(rtt)
	}
}

// ----------------------------------------------------------

type AnyTransportHandler struct {
	transport.UnimplementedHandler
	p *ParticipantImpl
}

func (h AnyTransportHandler) OnFailed(_isShortLived bool, _ici *types.ICEConnectionInfo) {
	h.p.onAnyTransportFailed()
}

func (h AnyTransportHandler) OnNegotiationFailed() {
	h.p.onAnyTransportNegotiationFailed()
}

func (h AnyTransportHandler) OnICECandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) error {
	return h.p.onICECandidate(c, target)
}

// ----------------------------------------------------------

type PublisherTransportHandler struct {
	AnyTransportHandler
}

func (h PublisherTransportHandler) OnAnswer(sd webrtc.SessionDescription) error {
	return h.p.onPublisherAnswer(sd)
}

func (h PublisherTransportHandler) OnTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	h.p.onMediaTrack(track, rtpReceiver)
}

func (h PublisherTransportHandler) OnInitialConnected() {
	h.p.onPublisherInitialConnected()
}

func (h PublisherTransportHandler) OnDataPacket(kind livekit.DataPacket_Kind, data []byte) {
	h.p.onDataMessage(kind, data)
}

// ----------------------------------------------------------

type SubscriberTransportHandler struct {
	AnyTransportHandler
}

func (h SubscriberTransportHandler) OnOffer(sd webrtc.SessionDescription) error {
	return h.p.onSubscriberOffer(sd)
}

func (h SubscriberTransportHandler) OnStreamStateChange(update *streamallocator.StreamStateUpdate) error {
	return h.p.onStreamStateChange(update)
}

func (h SubscriberTransportHandler) OnInitialConnected() {
	h.p.onSubscriberInitialConnected()
}

func (h SubscriberTransportHandler) OnDataSendError(err error) {
	h.p.onDataSendError(err)
}

// ----------------------------------------------------------

type PrimaryTransportHandler struct {
	transport.Handler
	p *ParticipantImpl
}

func (h PrimaryTransportHandler) OnInitialConnected() {
	h.Handler.OnInitialConnected()
	h.p.onPrimaryTransportInitialConnected()
}

func (h PrimaryTransportHandler) OnFullyEstablished() {
	h.p.onPrimaryTransportFullyEstablished()
}

// ----------------------------------------------------------

func (p *ParticipantImpl) setupTransportManager() error {
	p.twcc = twcc.NewTransportWideCCResponder()
	p.twcc.OnFeedback(func(pkts []rtcp.Packet) {
		p.postRtcp(pkts)
	})
	ath := AnyTransportHandler{p: p}
	var pth transport.Handler = PublisherTransportHandler{ath}
	var sth transport.Handler = SubscriberTransportHandler{ath}

	subscriberAsPrimary := p.ProtocolVersion().SubscriberAsPrimary() && p.CanSubscribe() && !p.params.UseOneShotSignallingMode
	if subscriberAsPrimary {
		sth = PrimaryTransportHandler{sth, p}
	} else {
		pth = PrimaryTransportHandler{pth, p}
	}

	params := TransportManagerParams{
		Identity: p.params.Identity,
		SID:      p.params.SID,
		// primary connection does not change, canSubscribe can change if permission was updated
		// after the participant has joined
		SubscriberAsPrimary:          subscriberAsPrimary,
		Config:                       p.params.Config,
		Twcc:                         p.twcc,
		ProtocolVersion:              p.params.ProtocolVersion,
		CongestionControlConfig:      p.params.CongestionControlConfig,
		EnabledPublishCodecs:         p.enabledPublishCodecs,
		EnabledSubscribeCodecs:       p.enabledSubscribeCodecs,
		SimTracks:                    p.params.SimTracks,
		ClientInfo:                   p.params.ClientInfo,
		Migration:                    p.params.Migration,
		AllowTCPFallback:             p.params.AllowTCPFallback,
		TCPFallbackRTTThreshold:      p.params.TCPFallbackRTTThreshold,
		AllowUDPUnstableFallback:     p.params.AllowUDPUnstableFallback,
		TURNSEnabled:                 p.params.TURNSEnabled,
		AllowPlayoutDelay:            p.params.PlayoutDelay.GetEnabled(),
		DataChannelMaxBufferedAmount: p.params.DataChannelMaxBufferedAmount,
		Logger:                       p.params.Logger.WithComponent(sutils.ComponentTransport),
		PublisherHandler:             pth,
		SubscriberHandler:            sth,
		DataChannelStats:             p.dataChannelStats,
		UseSendSideBWEInterceptor:    p.params.UseSendSideBWEInterceptor,
		UseSendSideBWE:               p.params.UseSendSideBWE,
		UseOneShotSignallingMode:     p.params.UseOneShotSignallingMode,
	}
	if p.params.SyncStreams && p.params.PlayoutDelay.GetEnabled() && p.params.ClientInfo.isFirefox() {
		// we will disable playout delay for Firefox if the user is expecting
		// the streams to be synced. Firefox doesn't support SyncStreams
		params.AllowPlayoutDelay = false
	}
	tm, err := NewTransportManager(params)
	if err != nil {
		return err
	}

	tm.OnICEConfigChanged(func(iceConfig *livekit.ICEConfig) {
		p.lock.Lock()
		onICEConfigChanged := p.onICEConfigChanged

		if p.params.ClientConf == nil {
			p.params.ClientConf = &livekit.ClientConfiguration{}
		}
		if iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TLS {
			p.params.ClientConf.ForceRelay = livekit.ClientConfigSetting_ENABLED
		} else {
			// UNSET indicates that clients could override RTCConfiguration to forceRelay
			p.params.ClientConf.ForceRelay = livekit.ClientConfigSetting_UNSET
		}
		p.lock.Unlock()

		if onICEConfigChanged != nil {
			onICEConfigChanged(p, iceConfig)
		}
	})

	tm.SetSubscriberAllowPause(p.params.SubscriberAllowPause)
	p.TransportManager = tm
	return nil
}

func (p *ParticipantImpl) setupUpTrackManager() {
	p.UpTrackManager = NewUpTrackManager(UpTrackManagerParams{
		SID:              p.params.SID,
		Logger:           p.pubLogger,
		VersionGenerator: p.params.VersionGenerator,
	})

	p.UpTrackManager.OnPublishedTrackUpdated(func(track types.MediaTrack) {
		p.dirty.Store(true)
		if onTrackUpdated := p.getOnTrackUpdated(); onTrackUpdated != nil {
			onTrackUpdated(p, track)
		}
	})

	p.UpTrackManager.OnUpTrackManagerClose(p.onUpTrackManagerClose)
}

func (p *ParticipantImpl) setupSubscriptionManager() {
	p.SubscriptionManager = NewSubscriptionManager(SubscriptionManagerParams{
		Participant:              p,
		Logger:                   p.subLogger.WithoutSampler(),
		TrackResolver:            p.params.TrackResolver,
		Telemetry:                p.params.Telemetry,
		OnTrackSubscribed:        p.onTrackSubscribed,
		OnTrackUnsubscribed:      p.onTrackUnsubscribed,
		OnSubscriptionError:      p.onSubscriptionError,
		SubscriptionLimitVideo:   p.params.SubscriptionLimitVideo,
		SubscriptionLimitAudio:   p.params.SubscriptionLimitAudio,
		UseOneShotSignallingMode: p.params.UseOneShotSignallingMode,
	})
}

func (p *ParticipantImpl) updateState(state livekit.ParticipantInfo_State) {
	if state == livekit.ParticipantInfo_ACTIVE {
		t := time.Now()
		p.lastActiveAt.CompareAndSwap(nil, &t)
	}
	oldState := p.state.Swap(state).(livekit.ParticipantInfo_State)
	if oldState == state {
		return
	}

	if state == livekit.ParticipantInfo_DISCONNECTED && oldState == livekit.ParticipantInfo_ACTIVE {
		prometheus.RecordSessionDuration(int(p.ProtocolVersion()), time.Since(*p.lastActiveAt.Load()))
	}
	p.params.Logger.Debugw("updating participant state", "state", state.String())
	p.dirty.Store(true)

	if onStateChange := p.getOnStateChange(); onStateChange != nil {
		go onStateChange(p, state)
	}
}

func (p *ParticipantImpl) setIsPublisher(isPublisher bool) {
	if p.isPublisher.Swap(isPublisher) == isPublisher {
		return
	}

	p.lock.Lock()
	p.requireBroadcast = true
	p.lock.Unlock()

	p.dirty.Store(true)

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

// when the server has an offer for participant
func (p *ParticipantImpl) onSubscriberOffer(offer webrtc.SessionDescription) error {
	p.subLogger.Debugw("sending offer", "transport", livekit.SignalTarget_SUBSCRIBER, "offer", offer)
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
}

func (p *ParticipantImpl) removePublishedTrack(track types.MediaTrack) {
	p.RemovePublishedTrack(track, false, true)
	if p.ProtocolVersion().SupportsUnpublish() {
		p.sendTrackUnpublished(track.ID())
	} else {
		// for older clients that don't support unpublish, mute to avoid them sending data
		p.sendTrackMuted(track.ID(), true)
	}
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	if p.IsDisconnected() {
		return
	}

	publishedTrack, isNewTrack := p.mediaTrackReceived(track, rtpReceiver)
	if publishedTrack == nil {
		p.pendingTracksLock.Lock()
		p.pendingRemoteTracks = append(p.pendingRemoteTracks, &pendingRemoteTrack{track: track, receiver: rtpReceiver})
		p.pendingTracksLock.Unlock()
		p.pubLogger.Debugw("webrtc Track published but can't find MediaTrack, add to pendingTracks",
			"kind", track.Kind().String(),
			"webrtcTrackID", track.ID(),
			"rid", track.RID(),
			"SSRC", track.SSRC(),
			"mime", track.Codec().MimeType,
		)
		return
	}

	if !p.CanPublishSource(publishedTrack.Source()) {
		p.pubLogger.Warnw("no permission to publish mediaTrack", nil,
			"source", publishedTrack.Source(),
		)
		p.removePublishedTrack(publishedTrack)
		return
	}

	p.setIsPublisher(true)
	p.dirty.Store(true)

	p.pubLogger.Infow("mediaTrack published",
		"kind", track.Kind().String(),
		"trackID", publishedTrack.ID(),
		"webrtcTrackID", track.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC(),
		"mime", track.Codec().MimeType,
		"trackInfo", logger.Proto(publishedTrack.ToProto()),
	)

	if !isNewTrack && !publishedTrack.HasPendingCodec() && p.IsReady() {
		if onTrackUpdated := p.getOnTrackUpdated(); onTrackUpdated != nil {
			onTrackUpdated(p, publishedTrack)
		}
	}
}

func (p *ParticipantImpl) handlePendingRemoteTracks() {
	p.pendingTracksLock.Lock()
	pendingTracks := p.pendingRemoteTracks
	p.pendingRemoteTracks = nil
	p.pendingTracksLock.Unlock()
	for _, rt := range pendingTracks {
		p.onMediaTrack(rt.track, rt.receiver)
	}
}

func (p *ParticipantImpl) onDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	if p.IsDisconnected() || !p.CanPublishData() {
		return
	}

	p.dataChannelStats.AddBytes(uint64(len(data)), false)

	dp := &livekit.DataPacket{}
	if err := proto.Unmarshal(data, dp); err != nil {
		p.pubLogger.Warnw("could not parse data packet", err)
		return
	}

	// trust the channel that it came in as the source of truth
	dp.Kind = kind

	shouldForwardData := true
	shouldForwardMetrics := false
	overrideSenderIdentity := true
	isPublisher := true
	// only forward on user payloads
	switch payload := dp.Value.(type) {
	case *livekit.DataPacket_User:
		if payload.User == nil {
			return
		}
		u := payload.User
		if p.Hidden() {
			u.ParticipantSid = ""
			u.ParticipantIdentity = ""
		} else {
			u.ParticipantSid = string(p.params.SID)
			u.ParticipantIdentity = string(p.params.Identity)
		}
		if len(dp.DestinationIdentities) != 0 {
			u.DestinationIdentities = dp.DestinationIdentities
		} else {
			dp.DestinationIdentities = u.DestinationIdentities
		}
	case *livekit.DataPacket_SipDtmf:
		if payload.SipDtmf == nil {
			return
		}
	case *livekit.DataPacket_Transcription:
		if payload.Transcription == nil {
			return
		}
		if !p.IsAgent() {
			shouldForwardData = false
		}
	case *livekit.DataPacket_ChatMessage:
		if payload.ChatMessage == nil {
			return
		}
		if p.IsAgent() && dp.ParticipantIdentity != "" && string(p.params.Identity) != dp.ParticipantIdentity {
			overrideSenderIdentity = false
			payload.ChatMessage.Generated = true
		}
	case *livekit.DataPacket_Metrics:
		if payload.Metrics == nil {
			return
		}
		shouldForwardData = false
		shouldForwardMetrics = true
		isPublisher = false
		// METRICS-TODO-QUESTIONS:
		// 1. Should this record (and do processing/batching) metrics (i. e. publisher side) rather
		//    than forwarding and recording/processing/batching at every subscriber (in this case
		//    subscriber is defined as the other participants pushing this to edge client).
		// 2. If the above is done, there could be two cadences, publisher side recording/processing/batching
		//    and pushing it to all subscribers on some cadence and subscribers have their own cadence of
		//    processing/batching and sending to edge clients.
		p.metricTimestamper.Process(payload.Metrics)
	case *livekit.DataPacket_RpcRequest:
		if payload.RpcRequest == nil {
			return
		}
		p.pubLogger.Infow("received RPC request data packet", "method", payload.RpcRequest.Method, "rpc_request_id", payload.RpcRequest.Id)
	case *livekit.DataPacket_RpcResponse:
		if payload.RpcResponse == nil {
			return
		}
	case *livekit.DataPacket_RpcAck:
		if payload.RpcAck == nil {
			return
		}
	default:
		p.pubLogger.Warnw("received unsupported data packet", nil, "payload", payload)
	}

	if p.Hidden() {
		dp.ParticipantIdentity = ""
	} else if overrideSenderIdentity {
		dp.ParticipantIdentity = string(p.params.Identity)
	}

	if shouldForwardData {
		if onDataPacket := p.getOnDataPacket(); onDataPacket != nil {
			onDataPacket(p, kind, dp)
		}
	}
	if shouldForwardMetrics {
		if onMetrics := p.getOnMetrics(); onMetrics != nil {
			onMetrics(p, dp)
		}
	}

	if isPublisher {
		p.setIsPublisher(true)
	}
}

func (p *ParticipantImpl) onICECandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) error {
	if p.IsDisconnected() || p.IsClosed() {
		return nil
	}

	if target == livekit.SignalTarget_SUBSCRIBER && p.MigrateState() == types.MigrateStateInit {
		return nil
	}

	return p.sendICECandidate(c, target)
}

func (p *ParticipantImpl) onPublisherInitialConnected() {
	p.SetMigrateState(types.MigrateStateComplete)

	if p.supervisor != nil {
		p.supervisor.SetPublisherPeerConnectionConnected(true)
	}

	if p.params.UseOneShotSignallingMode {
		go p.subscriberRTCPWorker()

		p.setDownTracksConnected()
	}

	p.pubRTCPQueue.Start()
}

func (p *ParticipantImpl) onSubscriberInitialConnected() {
	go p.subscriberRTCPWorker()

	p.setDownTracksConnected()
}

func (p *ParticipantImpl) onPrimaryTransportInitialConnected() {
	if !p.hasPendingMigratedTrack() && len(p.GetPublishedTracks()) == 0 {
		// if there are no published tracks, declare migration complete on primary transport initial connect,
		// else, wait for all tracks to be published and publisher peer connection established
		p.SetMigrateState(types.MigrateStateComplete)
	}
}

func (p *ParticipantImpl) onPrimaryTransportFullyEstablished() {
	if !p.sessionStartRecorded.Swap(true) {
		prometheus.RecordSessionStartTime(int(p.ProtocolVersion()), time.Since(p.params.SessionStartTime))
	}
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

		if p.IsClosed() || p.IsDisconnected() {
			return
		}
		_ = p.Close(true, types.ParticipantCloseReasonPeerConnectionDisconnected, false)
	})
	p.lock.Unlock()
}

func (p *ParticipantImpl) onAnyTransportFailed() {
	if p.params.UseOneShotSignallingMode {
		// as there is no way to notify participant, close the participant on transport failure
		_ = p.Close(false, types.ParticipantCloseReasonPeerConnectionDisconnected, false)
		return
	}

	p.sendLeaveRequest(
		types.ParticipantCloseReasonPeerConnectionDisconnected,
		true,  // isExpectedToResume
		false, // isExpectedToReconnect
		true,  // sendOnlyIfSupportingLeaveRequestWithAction
	)

	// clients support resuming of connections when signalling becomes disconnected
	p.CloseSignalConnection(types.SignallingCloseReasonTransportFailure)

	// detect when participant has actually left.
	p.setupDisconnectTimer()
}

// subscriberRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other publishedTracks in the room.
func (p *ParticipantImpl) subscriberRTCPWorker() {
	defer func() {
		if r := Recover(p.GetLogger()); r != nil {
			os.Exit(1)
		}
	}()
	for {
		if p.IsDisconnected() {
			return
		}

		subscribedTracks := p.SubscriptionManager.GetSubscribedTracks()

		// send in batches of sdBatchSize
		batchSize := 0
		var pkts []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		for _, subTrack := range subscribedTracks {
			sr := subTrack.DownTrack().CreateSenderReport()
			chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
			if sr == nil || chunks == nil {
				continue
			}

			pkts = append(pkts, sr)
			sd = append(sd, chunks...)
			numItems := 0
			for _, chunk := range chunks {
				numItems += len(chunk.Items)
			}
			batchSize = batchSize + 1 + numItems
			if batchSize >= sdBatchSize {
				if len(sd) != 0 {
					pkts = append(pkts, &rtcp.SourceDescription{Chunks: sd})
				}
				if err := p.TransportManager.WriteSubscriberRTCP(pkts); err != nil {
					if IsEOF(err) {
						return
					}
					p.subLogger.Errorw("could not send down track reports", err)
				}

				pkts = pkts[:0]
				sd = sd[:0]
				batchSize = 0
			}
		}

		if len(pkts) != 0 || len(sd) != 0 {
			if len(sd) != 0 {
				pkts = append(pkts, &rtcp.SourceDescription{Chunks: sd})
			}
			if err := p.TransportManager.WriteSubscriberRTCP(pkts); err != nil {
				if IsEOF(err) {
					return
				}
				p.subLogger.Errorw("could not send down track reports", err)
			}
		}

		time.Sleep(3 * time.Second)
	}
}

func (p *ParticipantImpl) onStreamStateChange(update *streamallocator.StreamStateUpdate) error {
	if len(update.StreamStates) == 0 {
		return nil
	}

	streamStateUpdate := &livekit.StreamStateUpdate{}
	for _, streamStateInfo := range update.StreamStates {
		state := livekit.StreamState_ACTIVE
		if streamStateInfo.State == streamallocator.StreamStatePaused {
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

func (p *ParticipantImpl) onSubscribedMaxQualityChange(
	trackID livekit.TrackID,
	trackInfo *livekit.TrackInfo,
	subscribedQualities []*livekit.SubscribedCodec,
	maxSubscribedQualities []types.SubscribedCodecQuality,
) error {
	if p.params.DisableDynacast {
		return nil
	}

	if len(subscribedQualities) == 0 {
		return nil
	}

	// send layer info about max subscription changes to telemetry
	for _, maxSubscribedQuality := range maxSubscribedQualities {
		ti := &livekit.TrackInfo{
			Sid:  trackInfo.Sid,
			Type: trackInfo.Type,
		}
		for _, layer := range trackInfo.Layers {
			if layer.Quality == maxSubscribedQuality.Quality {
				ti.Width = layer.Width
				ti.Height = layer.Height
				break
			}
		}
		p.params.Telemetry.TrackMaxSubscribedVideoQuality(
			context.Background(),
			p.ID(),
			ti,
			maxSubscribedQuality.CodecMime,
			maxSubscribedQuality.Quality,
		)
	}

	// normalize the codec name
	for _, subscribedQuality := range subscribedQualities {
		subscribedQuality.Codec = strings.ToLower(strings.TrimPrefix(subscribedQuality.Codec, "video/"))
	}

	subscribedQualityUpdate := &livekit.SubscribedQualityUpdate{
		TrackSid:            string(trackID),
		SubscribedQualities: subscribedQualities[0].Qualities, // for compatible with old client
		SubscribedCodecs:    subscribedQualities,
	}

	p.pubLogger.Debugw(
		"sending max subscribed quality",
		"trackID", trackID,
		"qualities", subscribedQualities,
		"max", maxSubscribedQualities,
	)
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscribedQualityUpdate{
			SubscribedQualityUpdate: subscribedQualityUpdate,
		},
	})
}

func (p *ParticipantImpl) addPendingTrackLocked(req *livekit.AddTrackRequest) *livekit.TrackInfo {
	if req.Sid != "" {
		track := p.GetPublishedTrack(livekit.TrackID(req.Sid))
		if track == nil {
			p.pubLogger.Infow("could not find existing track for multi-codec simulcast", "trackID", req.Sid)
			return nil
		}

		track.(*MediaTrack).UpdateCodecCid(req.SimulcastCodecs)
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
		Encryption: req.Encryption,
		Stream:     req.Stream,
	}
	if req.Stereo {
		ti.AudioFeatures = append(ti.AudioFeatures, livekit.AudioTrackFeature_TF_STEREO)
	}
	if req.DisableDtx {
		ti.AudioFeatures = append(ti.AudioFeatures, livekit.AudioTrackFeature_TF_NO_DTX)
	}
	if ti.Stream == "" {
		ti.Stream = StreamFromTrackSource(ti.Source)
	}
	p.setTrackID(req.Cid, ti)

	if len(req.SimulcastCodecs) == 0 {
		if req.Type == livekit.TrackType_VIDEO {
			// clients not supporting simulcast codecs, synthesise a codec
			ti.Codecs = append(ti.Codecs, &livekit.SimulcastCodecInfo{
				Cid:    req.Cid,
				Layers: req.Layers,
			})
		}
	} else {
		seenCodecs := make(map[string]struct{})
		for _, codec := range req.SimulcastCodecs {
			mime := codec.Codec
			if req.Type == livekit.TrackType_VIDEO {
				if !strings.HasPrefix(mime, "video/") {
					mime = "video/" + mime
				}
				if !IsCodecEnabled(p.enabledPublishCodecs, webrtc.RTPCodecCapability{MimeType: mime}) {
					altCodec := selectAlternativeVideoCodec(p.enabledPublishCodecs)
					p.pubLogger.Infow("falling back to alternative codec",
						"codec", mime,
						"altCodec", altCodec,
						"trackID", ti.Sid,
					)
					// select an alternative MIME type that's generally supported
					mime = altCodec
				}
			} else if req.Type == livekit.TrackType_AUDIO && !strings.HasPrefix(mime, "audio/") {
				mime = "audio/" + mime
			}

			if _, ok := seenCodecs[mime]; ok || mime == "" {
				continue
			}
			seenCodecs[mime] = struct{}{}

			clonedLayers := make([]*livekit.VideoLayer, 0, len(req.Layers))
			for _, l := range req.Layers {
				clonedLayers = append(clonedLayers, utils.CloneProto(l))
			}
			ti.Codecs = append(ti.Codecs, &livekit.SimulcastCodecInfo{
				MimeType: mime,
				Cid:      codec.Cid,
				Layers:   clonedLayers,
			})
		}
	}

	p.params.Telemetry.TrackPublishRequested(context.Background(), p.ID(), p.Identity(), ti)
	if p.supervisor != nil {
		p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
		p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)
	}
	if p.getPublishedTrackBySignalCid(req.Cid) != nil || p.getPublishedTrackBySdpCid(req.Cid) != nil || p.pendingTracks[req.Cid] != nil {
		if p.pendingTracks[req.Cid] == nil {
			p.pendingTracks[req.Cid] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}, createdAt: time.Now(), queued: true}
		} else {
			p.pendingTracks[req.Cid].trackInfos = append(p.pendingTracks[req.Cid].trackInfos, ti)
		}
		p.pubLogger.Infow("pending track queued", "trackID", ti.Sid, "track", logger.Proto(ti), "request", logger.Proto(req))
		return nil
	}

	p.pendingTracks[req.Cid] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}, createdAt: time.Now()}
	p.pubLogger.Debugw("pending track added", "trackID", ti.Sid, "track", logger.Proto(ti), "request", logger.Proto(req))
	return ti
}

func (p *ParticipantImpl) GetPendingTrack(trackID livekit.TrackID) *livekit.TrackInfo {
	p.pendingTracksLock.RLock()
	defer p.pendingTracksLock.RUnlock()

	for _, t := range p.pendingTracks {
		if livekit.TrackID(t.trackInfos[0].Sid) == trackID {
			return t.trackInfos[0]
		}
	}

	return nil
}

func (p *ParticipantImpl) HasConnected() bool {
	return p.TransportManager.HasSubscriberEverConnected() || p.TransportManager.HasPublisherEverConnected()
}

func (p *ParticipantImpl) sendTrackPublished(cid string, ti *livekit.TrackInfo) {
	p.pubLogger.Debugw("sending track published", "cid", cid, "trackInfo", logger.Proto(ti))
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   cid,
				Track: ti,
			},
		},
	})
}

func (p *ParticipantImpl) SetTrackMuted(trackID livekit.TrackID, muted bool, fromAdmin bool) *livekit.TrackInfo {
	// when request is coming from admin, send message to current participant
	if fromAdmin {
		p.sendTrackMuted(trackID, muted)
	}

	return p.setTrackMuted(trackID, muted)
}

func (p *ParticipantImpl) setTrackMuted(trackID livekit.TrackID, muted bool) *livekit.TrackInfo {
	p.dirty.Store(true)
	if p.supervisor != nil {
		p.supervisor.SetPublicationMute(trackID, muted)
	}

	track := p.UpTrackManager.SetPublishedTrackMuted(trackID, muted)
	var trackInfo *livekit.TrackInfo
	if track != nil {
		trackInfo = track.ToProto()
	}

	isPending := false
	p.pendingTracksLock.RLock()
	for _, pti := range p.pendingTracks {
		for _, ti := range pti.trackInfos {
			if livekit.TrackID(ti.Sid) == trackID {
				ti.Muted = muted
				isPending = true
				trackInfo = ti
			}
		}
	}
	p.pendingTracksLock.RUnlock()

	if trackInfo != nil {
		if muted {
			p.params.Telemetry.TrackMuted(context.Background(), p.ID(), trackInfo)
		} else {
			p.params.Telemetry.TrackUnmuted(context.Background(), p.ID(), trackInfo)
		}
	}

	if !isPending && track == nil {
		p.pubLogger.Debugw("could not locate track", "trackID", trackID)
	}

	return trackInfo
}

func (p *ParticipantImpl) mediaTrackReceived(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) (*MediaTrack, bool) {
	p.pendingTracksLock.Lock()
	newTrack := false

	mid := p.TransportManager.GetPublisherMid(rtpReceiver)
	p.pubLogger.Debugw(
		"media track received",
		"kind", track.Kind().String(),
		"trackID", track.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC(),
		"mime", track.Codec().MimeType,
		"mid", mid,
	)
	if mid == "" {
		p.pendingTracksLock.Unlock()
		p.pubLogger.Warnw("could not get mid for track", nil, "trackID", track.ID())
		return nil, false
	}

	// use existing media track to handle simulcast
	var pubTime time.Duration
	mt, ok := p.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti, migrated, createdAt := p.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()), true)
		if ti == nil {
			p.pendingTracksLock.Unlock()
			return nil, false
		}

		// check if the migrated track has correct codec
		if migrated && len(ti.Codecs) > 0 {
			parameters := rtpReceiver.GetParameters()
			var codecFound int
			for _, c := range ti.Codecs {
				for _, nc := range parameters.Codecs {
					if strings.EqualFold(nc.MimeType, c.MimeType) {
						codecFound++
						break
					}
				}
			}
			if codecFound != len(ti.Codecs) {
				p.pubLogger.Warnw("migrated track codec mismatched", nil, "track", logger.Proto(ti), "webrtcCodec", parameters)
				p.pendingTracksLock.Unlock()
				p.IssueFullReconnect(types.ParticipantCloseReasonMigrateCodecMismatch)
				return nil, false
			}
		}

		ti.MimeType = track.Codec().MimeType
		// set mime_type for tracks that the AddTrackRequest do not have simulcast_codecs set
		if len(ti.Codecs) == 1 && ti.Codecs[0].MimeType == "" {
			ti.Codecs[0].MimeType = track.Codec().MimeType
		}
		if utils.TimedVersionFromProto(ti.Version).IsZero() {
			// only assign version on a fresh publish, i. e. avoid updating version in scenarios like migration
			ti.Version = p.params.VersionGenerator.Next().ToProto()
		}
		mt = p.addMediaTrack(signalCid, track.ID(), ti)
		newTrack = true

		// if the addTrackRequest is sent before participant active then it means the client tries to publish
		// before fully connected, in this case we only record the time when the participant is active since
		// we want this metric to represent the time cost by pubilshing.
		if activeAt := p.lastActiveAt.Load(); activeAt != nil && createdAt.Before(*activeAt) {
			createdAt = *activeAt
		}
		pubTime = time.Since(createdAt)
		p.dirty.Store(true)
	}

	p.pendingTracksLock.Unlock()

	mt.AddReceiver(rtpReceiver, track, mid)

	if newTrack {
		go func() {
			// TODO: remove this after we know where the high delay is coming from
			if pubTime > 3*time.Second {
				p.pubLogger.Infow(
					"track published with high delay",
					"trackID", mt.ID(),
					"track", logger.Proto(mt.ToProto()),
					"cost", pubTime.Milliseconds(),
					"rid", track.RID(),
					"mime", track.Codec().MimeType,
				)
			} else {
				p.pubLogger.Debugw(
					"track published",
					"trackID", mt.ID(),
					"track", logger.Proto(mt.ToProto()),
					"cost", pubTime.Milliseconds(),
				)
			}

			prometheus.RecordPublishTime(mt.Source(), mt.Kind(), pubTime, p.GetClientInfo().GetSdk(), p.Kind())
			p.handleTrackPublished(mt)
		}()
	}

	return mt, newTrack
}

func (p *ParticipantImpl) addMigratedTrack(cid string, ti *livekit.TrackInfo) *MediaTrack {
	p.pubLogger.Infow("add migrated track", "cid", cid, "trackID", ti.Sid, "track", logger.Proto(ti))
	rtpReceiver := p.TransportManager.GetPublisherRTPReceiver(ti.Mid)
	if rtpReceiver == nil {
		p.pubLogger.Errorw("could not find receiver for migrated track", nil, "trackID", ti.Sid, "mid", ti.Mid)
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
	// check for mime_type for tracks that do not have simulcast_codecs set
	if ti.MimeType != "" {
		for _, nc := range parameters.Codecs {
			if strings.EqualFold(nc.MimeType, ti.MimeType) {
				alreadyAdded := false
				for _, pc := range potentialCodecs {
					if strings.EqualFold(pc.MimeType, ti.MimeType) {
						alreadyAdded = true
						break
					}
				}
				if !alreadyAdded {
					potentialCodecs = append(potentialCodecs, nc)
				}
				break
			}
		}
	}
	mt.SetPotentialCodecs(potentialCodecs, parameters.HeaderExtensions)

	for _, codec := range ti.Codecs {
		for ssrc, info := range p.params.SimTracks {
			if info.Mid == codec.Mid {
				mt.SetLayerSsrc(codec.MimeType, info.Rid, ssrc)
			}
		}
	}
	mt.SetSimulcast(ti.Simulcast)
	mt.SetMuted(ti.Muted)

	return mt
}

func (p *ParticipantImpl) addMediaTrack(signalCid string, sdpCid string, ti *livekit.TrackInfo) *MediaTrack {
	mt := NewMediaTrack(MediaTrackParams{
		SignalCid:             signalCid,
		SdpCid:                sdpCid,
		ParticipantID:         p.params.SID,
		ParticipantIdentity:   p.params.Identity,
		ParticipantVersion:    p.version.Load(),
		BufferFactory:         p.params.Config.BufferFactory,
		ReceiverConfig:        p.params.Config.Receiver,
		AudioConfig:           p.params.AudioConfig,
		VideoConfig:           p.params.VideoConfig,
		Telemetry:             p.params.Telemetry,
		Logger:                LoggerWithTrack(p.pubLogger, livekit.TrackID(ti.Sid), false),
		SubscriberConfig:      p.params.Config.Subscriber,
		PLIThrottleConfig:     p.params.PLIThrottleConfig,
		SimTracks:             p.params.SimTracks,
		OnRTCP:                p.postRtcp,
		ForwardStats:          p.params.ForwardStats,
		OnTrackEverSubscribed: p.sendTrackHasBeenSubscribed,
	}, ti)

	mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)

	// add to published and clean up pending
	if p.supervisor != nil {
		p.supervisor.SetPublishedTrack(livekit.TrackID(ti.Sid), mt)
	}
	p.UpTrackManager.AddPublishedTrack(mt)

	pti := p.pendingTracks[signalCid]
	if pti != nil {
		if p.pendingPublishingTracks[livekit.TrackID(ti.Sid)] != nil {
			p.pubLogger.Infow("unexpected pending publish track", "trackID", ti.Sid)
		}
		p.pendingPublishingTracks[livekit.TrackID(ti.Sid)] = &pendingTrackInfo{
			trackInfos: []*livekit.TrackInfo{pti.trackInfos[0]},
			migrated:   pti.migrated,
		}
	}

	p.pendingTracks[signalCid].trackInfos = p.pendingTracks[signalCid].trackInfos[1:]
	if len(p.pendingTracks[signalCid].trackInfos) == 0 {
		delete(p.pendingTracks, signalCid)
	} else {
		p.pendingTracks[signalCid].queued = true
		p.pendingTracks[signalCid].createdAt = time.Now()
	}

	trackID := livekit.TrackID(ti.Sid)
	mt.AddOnClose(func(_isExpectedToRsume bool) {
		if p.supervisor != nil {
			p.supervisor.ClearPublishedTrack(trackID, mt)
		}

		p.params.Telemetry.TrackUnpublished(
			context.Background(),
			p.ID(),
			p.Identity(),
			mt.ToProto(),
			true,
		)

		p.pendingTracksLock.Lock()
		if pti := p.pendingTracks[signalCid]; pti != nil {
			p.sendTrackPublished(signalCid, pti.trackInfos[0])
			pti.queued = false
		}
		p.pendingTracksLock.Unlock()
		p.handlePendingRemoteTracks()

		p.dirty.Store(true)

		p.pubLogger.Debugw("track unpublished", "trackID", ti.Sid, "track", logger.Proto(ti))
		if onTrackUnpublished := p.getOnTrackUnpublished(); onTrackUnpublished != nil {
			onTrackUnpublished(p, mt)
		}
	})

	return mt
}

func (p *ParticipantImpl) handleTrackPublished(track types.MediaTrack) {
	if onTrackPublished := p.getOnTrackPublished(); onTrackPublished != nil {
		onTrackPublished(p, track)
	}

	// send webhook after callbacks are complete, persistence and state handling happens
	// in `onTrackPublished` cb
	p.params.Telemetry.TrackPublished(
		context.Background(),
		p.ID(),
		p.Identity(),
		track.ToProto(),
	)

	p.pendingTracksLock.Lock()
	delete(p.pendingPublishingTracks, track.ID())
	p.pendingTracksLock.Unlock()
}

func (p *ParticipantImpl) hasPendingMigratedTrack() bool {
	p.pendingTracksLock.RLock()
	defer p.pendingTracksLock.RUnlock()

	for _, t := range p.pendingTracks {
		if t.migrated {
			return true
		}
	}

	for _, t := range p.pendingPublishingTracks {
		if t.migrated {
			return true
		}
	}

	return false
}

func (p *ParticipantImpl) onUpTrackManagerClose() {
	p.pubRTCPQueue.Stop()
}

func (p *ParticipantImpl) getPendingTrack(clientId string, kind livekit.TrackType, skipQueued bool) (string, *livekit.TrackInfo, bool, time.Time) {
	signalCid := clientId
	pendingInfo := p.pendingTracks[clientId]
	if pendingInfo == nil {
	track_loop:
		for cid, pti := range p.pendingTracks {
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
	if pendingInfo == nil || (skipQueued && pendingInfo.queued) {
		return signalCid, nil, false, time.Time{}
	}

	return signalCid, pendingInfo.trackInfos[0], pendingInfo.migrated, pendingInfo.createdAt
}

// setTrackID either generates a new TrackID for an AddTrackRequest
func (p *ParticipantImpl) setTrackID(cid string, info *livekit.TrackInfo) {
	var trackID string
	// if already pending, use the same SID
	// should not happen as this means multiple `AddTrack` requests have been called, but check anyway
	if pti := p.pendingTracks[cid]; pti != nil {
		trackID = pti.trackInfos[0].Sid
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
		trackID = guid.New(trackPrefix)
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
			p.pubLogger.Debugw("found track by SDP cid", "sdpCid", clientId, "trackID", publishedTrack.ID())
			return publishedTrack
		}
	}

	return nil
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

	return info
}

func (p *ParticipantImpl) postRtcp(pkts []rtcp.Packet) {
	p.lock.RLock()
	migrationTimer := p.migrationTimer
	p.lock.RUnlock()

	// Once migration out is active, layers getting added would not be communicated to
	// where the publisher is migrating to. Without SSRC, `UnhandleSimulcastInterceptor`
	// cannot be set up on the migrating in node. Without that interceptor, simulcast
	// probing will fail.
	//
	// Clients usually send `rid` RTP header extension till they get an RTCP Receiver Report
	// from the remote side. So, by curbing RTCP when migration is active, even if a new layer
	// get published to this node, client should continue to send `rid` to the new node
	// post migration and the new node can do regular simulcast probing (without the
	// `UnhandleSimulcastInterceptor`) to fire `OnTrack` on that layer. And when the new node
	// sends RTCP Receiver Report back to the client, client will stop `rid`.
	if migrationTimer != nil {
		return
	}

	p.pubRTCPQueue.Enqueue(func(op postRtcpOp) {
		if err := op.TransportManager.WritePublisherRTCP(op.pkts); err != nil && !IsEOF(err) {
			op.pubLogger.Errorw("could not write RTCP to participant", err)
		}
	}, postRtcpOp{p, pkts})
}

func (p *ParticipantImpl) setDownTracksConnected() {
	for _, t := range p.SubscriptionManager.GetSubscribedTracks() {
		if dt := t.DownTrack(); dt != nil {
			dt.SeedState(sfu.DownTrackState{ForwarderState: p.getAndDeleteForwarderState(t.ID())})
			dt.SetConnected()
		}
	}
}

func (p *ParticipantImpl) cacheForwarderState() {
	// if migrating in, get forwarder state from migrating out node to facilitate resume
	if f := p.params.GetSubscriberForwarderState; f != nil {
		if fs, err := f(p); err == nil {
			p.lock.Lock()
			p.forwarderState = fs
			p.lock.Unlock()
		}
	}
}

func (p *ParticipantImpl) getAndDeleteForwarderState(trackID livekit.TrackID) *livekit.RTPForwarderState {
	p.lock.Lock()
	fs := p.forwarderState[trackID]
	delete(p.forwarderState, trackID)
	p.lock.Unlock()

	return fs
}

func (p *ParticipantImpl) CacheDownTrack(trackID livekit.TrackID, rtpTransceiver *webrtc.RTPTransceiver, downTrack sfu.DownTrackState) {
	p.lock.Lock()
	if existing := p.cachedDownTracks[trackID]; existing != nil && existing.transceiver != rtpTransceiver {
		p.subLogger.Warnw("cached transceiver changed", nil, "trackID", trackID)
	}
	p.cachedDownTracks[trackID] = &downTrackState{transceiver: rtpTransceiver, downTrack: downTrack}
	p.subLogger.Debugw("caching downtrack", "trackID", trackID)
	p.lock.Unlock()
}

func (p *ParticipantImpl) UncacheDownTrack(rtpTransceiver *webrtc.RTPTransceiver) {
	p.lock.Lock()
	for trackID, dts := range p.cachedDownTracks {
		if dts.transceiver == rtpTransceiver {
			if dts := p.cachedDownTracks[trackID]; dts != nil {
				p.subLogger.Debugw("uncaching downtrack", "trackID", trackID)
			}
			delete(p.cachedDownTracks, trackID)
			break
		}
	}
	p.lock.Unlock()
}

func (p *ParticipantImpl) GetCachedDownTrack(trackID livekit.TrackID) (*webrtc.RTPTransceiver, sfu.DownTrackState) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if dts := p.cachedDownTracks[trackID]; dts != nil {
		return dts.transceiver, dts.downTrack
	}

	return nil, sfu.DownTrackState{}
}

func (p *ParticipantImpl) IssueFullReconnect(reason types.ParticipantCloseReason) {
	p.sendLeaveRequest(
		reason,
		false, // isExpectedToResume
		true,  // isExpectedToReconnect
		false, // sendOnlyIfSupportingLeaveRequestWithAction
	)

	scr := types.SignallingCloseReasonUnknown
	switch reason {
	case types.ParticipantCloseReasonPublicationError, types.ParticipantCloseReasonMigrateCodecMismatch:
		scr = types.SignallingCloseReasonFullReconnectPublicationError
	case types.ParticipantCloseReasonSubscriptionError:
		scr = types.SignallingCloseReasonFullReconnectSubscriptionError
	case types.ParticipantCloseReasonDataChannelError:
		scr = types.SignallingCloseReasonFullReconnectDataChannelError
	case types.ParticipantCloseReasonNegotiateFailed:
		scr = types.SignallingCloseReasonFullReconnectNegotiateFailed
	}
	p.CloseSignalConnection(scr)

	// a full reconnect == client should connect back with a new session, close current one
	p.Close(false, reason, false)
}

func (p *ParticipantImpl) onPublicationError(trackID livekit.TrackID) {
	if p.params.ReconnectOnPublicationError {
		p.pubLogger.Infow("issuing full reconnect on publication error", "trackID", trackID)
		p.IssueFullReconnect(types.ParticipantCloseReasonPublicationError)
	}
}

func (p *ParticipantImpl) onSubscriptionError(trackID livekit.TrackID, fatal bool, err error) {
	signalErr := livekit.SubscriptionError_SE_UNKNOWN
	switch {
	case errors.Is(err, webrtc.ErrUnsupportedCodec):
		signalErr = livekit.SubscriptionError_SE_CODEC_UNSUPPORTED
	case errors.Is(err, ErrTrackNotFound):
		signalErr = livekit.SubscriptionError_SE_TRACK_NOTFOUND
	}

	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscriptionResponse{
			SubscriptionResponse: &livekit.SubscriptionResponse{
				TrackSid: string(trackID),
				Err:      signalErr,
			},
		},
	})

	if p.params.ReconnectOnSubscriptionError && fatal {
		p.subLogger.Infow("issuing full reconnect on subscription error", "trackID", trackID)
		p.IssueFullReconnect(types.ParticipantCloseReasonSubscriptionError)
	}
}

func (p *ParticipantImpl) onAnyTransportNegotiationFailed() {
	if p.TransportManager.SinceLastSignal() < negotiationFailedTimeout/2 {
		p.params.Logger.Infow("negotiation failed, starting full reconnect")
	}
	p.IssueFullReconnect(types.ParticipantCloseReasonNegotiateFailed)
}

func (p *ParticipantImpl) UpdateSubscribedQuality(nodeID livekit.NodeID, trackID livekit.TrackID, maxQualities []types.SubscribedCodecQuality) error {
	track := p.GetPublishedTrack(trackID)
	if track == nil {
		p.pubLogger.Debugw("could not find track", "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.(types.LocalMediaTrack).NotifySubscriberNodeMaxQuality(nodeID, maxQualities)
	return nil
}

func (p *ParticipantImpl) UpdateMediaLoss(nodeID livekit.NodeID, trackID livekit.TrackID, fractionalLoss uint32) error {
	track := p.GetPublishedTrack(trackID)
	if track == nil {
		p.pubLogger.Debugw("could not find track", "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.(types.LocalMediaTrack).NotifySubscriberNodeMediaLoss(nodeID, uint8(fractionalLoss))
	return nil
}

func (p *ParticipantImpl) GetPlayoutDelayConfig() *livekit.PlayoutDelay {
	return p.params.PlayoutDelay
}

func (p *ParticipantImpl) SupportsSyncStreamID() bool {
	return p.ProtocolVersion().SupportSyncStreamID() && !p.params.ClientInfo.isFirefox() && p.params.SyncStreams
}

func (p *ParticipantImpl) SupportsTransceiverReuse() bool {
	return p.ProtocolVersion().SupportsTransceiverReuse() && !p.SupportsSyncStreamID()
}

func (p *ParticipantImpl) SendDataPacket(kind livekit.DataPacket_Kind, encoded []byte) error {
	if p.State() != livekit.ParticipantInfo_ACTIVE {
		return ErrDataChannelUnavailable
	}

	return p.TransportManager.SendDataPacket(kind, encoded)
}

func (p *ParticipantImpl) onDataSendError(err error) {
	if p.params.ReconnectOnDataChannelError {
		p.params.Logger.Infow("issuing full reconnect on data channel error", "error", err)
		p.IssueFullReconnect(types.ParticipantCloseReasonDataChannelError)
	}
}

func (p *ParticipantImpl) setupEnabledCodecs(publishEnabledCodecs []*livekit.Codec, subscribeEnabledCodecs []*livekit.Codec, disabledCodecs *livekit.DisabledCodecs) {
	shouldDisable := func(c *livekit.Codec, disabled []*livekit.Codec) bool {
		for _, disableCodec := range disabled {
			// disable codec's fmtp is empty means disable this codec entirely
			if strings.EqualFold(c.Mime, disableCodec.Mime) {
				return true
			}
		}
		return false
	}

	publishCodecs := make([]*livekit.Codec, 0, len(publishEnabledCodecs))
	for _, c := range publishEnabledCodecs {
		if shouldDisable(c, disabledCodecs.GetCodecs()) || shouldDisable(c, disabledCodecs.GetPublish()) {
			continue
		}

		// sort by compatibility, since we will look for backups in these.
		if strings.EqualFold(c.Mime, webrtc.MimeTypeVP8) {
			if len(p.enabledPublishCodecs) > 0 {
				p.enabledPublishCodecs = slices.Insert(p.enabledPublishCodecs, 0, c)
			} else {
				p.enabledPublishCodecs = append(p.enabledPublishCodecs, c)
			}
		} else if strings.EqualFold(c.Mime, webrtc.MimeTypeH264) {
			p.enabledPublishCodecs = append(p.enabledPublishCodecs, c)
		} else {
			publishCodecs = append(publishCodecs, c)
		}
	}
	p.enabledPublishCodecs = append(p.enabledPublishCodecs, publishCodecs...)

	subscribeCodecs := make([]*livekit.Codec, 0, len(subscribeEnabledCodecs))
	for _, c := range subscribeEnabledCodecs {
		if shouldDisable(c, disabledCodecs.GetCodecs()) {
			continue
		}
		subscribeCodecs = append(subscribeCodecs, c)
	}
	p.enabledSubscribeCodecs = subscribeCodecs
}

func (p *ParticipantImpl) GetEnabledPublishCodecs() []*livekit.Codec {
	codecs := make([]*livekit.Codec, 0, len(p.enabledPublishCodecs))
	for _, c := range p.enabledPublishCodecs {
		if c.Mime == "video/rtx" {
			continue
		}
		codecs = append(codecs, c)
	}
	return codecs
}

func (p *ParticipantImpl) UpdateAudioTrack(update *livekit.UpdateLocalAudioTrack) error {
	if track := p.UpTrackManager.UpdatePublishedAudioTrack(update); track != nil {
		return nil
	}

	isPending := false
	p.pendingTracksLock.RLock()
	for _, pti := range p.pendingTracks {
		for _, ti := range pti.trackInfos {
			if ti.Sid == update.TrackSid {
				isPending = true

				ti.AudioFeatures = update.Features
				ti.Stereo = false
				ti.DisableDtx = false
				for _, feature := range update.Features {
					switch feature {
					case livekit.AudioTrackFeature_TF_STEREO:
						ti.Stereo = true
					case livekit.AudioTrackFeature_TF_NO_DTX:
						ti.DisableDtx = true
					}
				}

				p.pubLogger.Debugw("updated pending track", "trackID", ti.Sid, "trackInfo", logger.Proto(ti))
			}
		}
	}
	p.pendingTracksLock.RUnlock()
	if isPending {
		return nil
	}

	p.pubLogger.Debugw("could not locate track", "trackID", update.TrackSid)
	return errors.New("could not find track")
}

func (p *ParticipantImpl) UpdateVideoTrack(update *livekit.UpdateLocalVideoTrack) error {
	if track := p.UpTrackManager.UpdatePublishedVideoTrack(update); track != nil {
		return nil
	}

	isPending := false
	p.pendingTracksLock.RLock()
	for _, pti := range p.pendingTracks {
		for _, ti := range pti.trackInfos {
			if ti.Sid == update.TrackSid {
				isPending = true

				ti.Width = update.Width
				ti.Height = update.Height
			}
		}
	}
	p.pendingTracksLock.RUnlock()
	if isPending {
		return nil
	}

	p.pubLogger.Debugw("could not locate track", "trackID", update.TrackSid)
	return errors.New("could not find track")
}

func (p *ParticipantImpl) HandleMetrics(senderParticipantID livekit.ParticipantID, metrics *livekit.MetricsBatch) error {
	if p.State() != livekit.ParticipantInfo_ACTIVE {
		return ErrDataChannelUnavailable
	}

	if !p.CanSubscribeMetrics() {
		return ErrNoSubscribeMetricsPermission
	}

	if !p.SubscriptionManager.IsSubscribedTo(senderParticipantID) {
		return nil
	}

	// METRICS-TODO:  This is just forwarding. Will have to do more, including but not limited to
	//   1. Filtering: subscriber metrics from self only should be sent to that participant.
	//   2. Batching: could include re-mapping labels to consolidate multiple batches.
	//   3. (Maybe) Time stamps: this is done on receive, TBD if required here also
	dpData, err := proto.Marshal(&livekit.DataPacket{
		Value: &livekit.DataPacket_Metrics{
			Metrics: metrics,
		},
	})
	if err != nil {
		p.params.Logger.Errorw("failed to marshal data packet", err)
		return err
	}

	return p.TransportManager.SendDataPacket(livekit.DataPacket_RELIABLE, dpData)
}

func (p *ParticipantImpl) StoreReliableDataPacketForLaterDelivery(dp *livekit.DataPacket) {
	p.reliableDataPacketsQueue = append(p.reliableDataPacketsQueue, dp)
}

func (p *ParticipantImpl) DeliverStoredReliableDataPackets() {
	for _, dp := range p.reliableDataPacketsQueue {
		var dpData, err = proto.Marshal(dp)
		if err != nil {
			logger.Errorw("failed to marshal data packet", err)
			continue
		}
		p.GetLogger().Debugw("resending stored reliable data packet", "source", dp.ParticipantIdentity, "destinationIdentities", dp.DestinationIdentities)
		p.SendDataPacket(livekit.DataPacket_RELIABLE, dpData)
	}
	p.reliableDataPacketsQueue = p.reliableDataPacketsQueue[:0]
}
  
// ----------------------------------------------

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