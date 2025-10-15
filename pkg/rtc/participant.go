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
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability"
	"github.com/livekit/protocol/observability/roomobs"
	protosdp "github.com/livekit/protocol/sdp"
	protosignalling "github.com/livekit/protocol/signalling"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/protocol/utils/pointer"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/metric"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/signalling"
	"github.com/livekit/livekit-server/pkg/rtc/supervisor"
	"github.com/livekit/livekit-server/pkg/rtc/transport"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/sfu/streamallocator"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	sutils "github.com/livekit/livekit-server/pkg/utils"
)

var _ types.LocalParticipant = (*ParticipantImpl)(nil)

const (
	sdBatchSize       = 30
	rttUpdateInterval = 5 * time.Second

	disconnectCleanupDuration          = 5 * time.Second
	migrationWaitDuration              = 3 * time.Second
	migrationWaitContinuousMsgDuration = 2 * time.Second

	PingIntervalSeconds = 5
	PingTimeoutSeconds  = 15
)

var (
	ErrMoveOldClientVersion = errors.New("participant client version does not support moving")
)

// -------------------------------------------------

type pendingTrackInfo struct {
	trackInfos []*livekit.TrackInfo
	sdpRids    buffer.VideoLayersRid
	migrated   bool
	createdAt  time.Time

	// indicates if this track is queued for publishing to avoid a track has been published
	// before the previous track is unpublished(closed) because client is allowed to negotiate
	// webrtc track before AddTrackRequest return to speed up the publishing process
	queued bool
}

func (p *pendingTrackInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if p == nil {
		return nil
	}

	e.AddArray("trackInfos", logger.ProtoSlice(p.trackInfos))
	e.AddArray("sdpRids", logger.StringSlice(p.sdpRids[:]))
	e.AddBool("migrated", p.migrated)
	e.AddTime("createdAt", p.createdAt)
	e.AddBool("queued", p.queued)
	return nil
}

// --------------------------------------------------

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

type reliableDataInfo struct {
	joiningMessageLock            sync.Mutex
	joiningMessageFirstSeqs       map[livekit.ParticipantID]uint32
	joiningMessageLastWrittenSeqs map[livekit.ParticipantID]uint32
	lastPubReliableSeq            atomic.Uint32
	stopReliableByMigrateOut      atomic.Bool
	canWriteReliable              bool
	migrateInPubDataCache         atomic.Pointer[MigrationDataCache]
}

// ---------------------------------------------------------------

var _ types.LocalParticipant = (*ParticipantImpl)(nil)

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
	LoggerResolver                 logger.DeferredFieldResolver
	Reporter                       roomobs.ParticipantSessionReporter
	ReporterResolver               roomobs.ParticipantReporterResolver
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
	ParticipantHelper              types.LocalParticipantHelper
	DisableSupervisor              bool
	ReconnectOnPublicationError    bool
	ReconnectOnSubscriptionError   bool
	ReconnectOnDataChannelError    bool
	VersionGenerator               utils.TimedVersionGenerator
	DisableDynacast                bool
	SubscriberAllowPause           bool
	SubscriptionLimitAudio         int32
	SubscriptionLimitVideo         int32
	PlayoutDelay                   *livekit.PlayoutDelay
	SyncStreams                    bool
	ForwardStats                   *sfu.ForwardStats
	DisableSenderReportPassThrough bool
	MetricConfig                   metric.MetricConfig
	UseOneShotSignallingMode       bool
	EnableMetrics                  bool
	DataChannelMaxBufferedAmount   uint64
	DatachannelSlowThreshold       int
	FireOnTrackBySdp               bool
	DisableCodecRegression         bool
	LastPubReliableSeq             uint32
	Country                        string
	PreferVideoSizeFromMedia       bool
	UseSinglePeerConnection        bool
}

type ParticipantImpl struct {
	// utils.TimedVersion is a atomic. To be correctly aligned also on 32bit archs
	// 64it atomics need to be at the front of a struct
	timedVersion utils.TimedVersion

	params ParticipantParams

	participantHelper atomic.Value // types.LocalParticipantHelper
	id                atomic.Value // types.ParticipantID

	isClosed    atomic.Bool
	closeReason atomic.Value // types.ParticipantCloseReason

	state        atomic.Value // livekit.ParticipantInfo_State
	disconnected chan struct{}

	grants      atomic.Pointer[auth.ClaimGrants]
	isPublisher atomic.Bool

	sessionStartRecorded atomic.Bool
	lastActiveAt         atomic.Pointer[time.Time]
	// when first connected
	connectedAt    time.Time
	disconnectedAt atomic.Pointer[time.Time]
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

	reliableDataInfo reliableDataInfo

	rttUpdatedAt time.Time
	lastRTT      uint32

	// idempotent reference guard for telemetry stats worker
	telemetryGuard *telemetry.ReferenceGuard

	lock utils.RWMutex

	dirty   atomic.Bool
	version atomic.Uint32

	// callbacks & handlers
	onTrackPublished               func(types.LocalParticipant, types.MediaTrack)
	onTrackUpdated                 func(types.LocalParticipant, types.MediaTrack)
	onTrackUnpublished             func(types.LocalParticipant, types.MediaTrack)
	onStateChange                  func(p types.LocalParticipant)
	onSubscriberReady              func(p types.LocalParticipant)
	onMigrateStateChange           func(p types.LocalParticipant, migrateState types.MigrateState)
	onParticipantUpdate            func(types.LocalParticipant)
	onDataPacket                   func(types.LocalParticipant, livekit.DataPacket_Kind, *livekit.DataPacket)
	onDataMessage                  func(types.LocalParticipant, []byte)
	onMetrics                      func(types.Participant, *livekit.DataPacket)
	onUpdateSubscriptions          func(types.LocalParticipant, []livekit.TrackID, []*livekit.ParticipantTracks, bool)
	onUpdateSubscriptionPermission func(types.LocalParticipant, *livekit.SubscriptionPermission) error
	onSyncState                    func(types.LocalParticipant, *livekit.SyncState) error
	onSimulateScenario             func(types.LocalParticipant, *livekit.SimulateScenario) error
	onLeave                        func(types.LocalParticipant, types.ParticipantCloseReason)

	migrateState                atomic.Value // types.MigrateState
	migratedTracksPublishedFuse core.Fuse

	onClose            map[string]func(types.LocalParticipant)
	onClaimsChanged    func(participant types.LocalParticipant)
	onICEConfigChanged func(participant types.LocalParticipant, iceConfig *livekit.ICEConfig)

	cachedDownTracks map[livekit.TrackID]*downTrackState
	forwarderState   map[livekit.TrackID]*livekit.RTPForwarderState

	supervisor *supervisor.ParticipantSupervisor

	connectionQuality livekit.ConnectionQuality

	metricTimestamper *metric.MetricTimestamper
	metricsCollector  *metric.MetricsCollector
	metricsReporter   *metric.MetricsReporter

	signalling    signalling.ParticipantSignalling
	signalHandler signalling.ParticipantSignalHandler
	signaller     signalling.ParticipantSignaller

	// loggers for publisher and subscriber
	pubLogger logger.Logger
	subLogger logger.Logger

	rpcLock             sync.Mutex
	rpcPendingAcks      map[string]*utils.DataChannelRpcPendingAckHandler
	rpcPendingResponses map[string]*utils.DataChannelRpcPendingResponseHandler
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
		connectedAt:             time.Now().Truncate(time.Millisecond),
		rttUpdatedAt:            time.Now(),
		cachedDownTracks:        make(map[livekit.TrackID]*downTrackState),
		connectionQuality:       livekit.ConnectionQuality_EXCELLENT,
		pubLogger:               params.Logger.WithComponent(sutils.ComponentPub),
		subLogger:               params.Logger.WithComponent(sutils.ComponentSub),
		reliableDataInfo: reliableDataInfo{
			joiningMessageFirstSeqs:       make(map[livekit.ParticipantID]uint32),
			joiningMessageLastWrittenSeqs: make(map[livekit.ParticipantID]uint32),
		},
		rpcPendingAcks:      make(map[string]*utils.DataChannelRpcPendingAckHandler),
		rpcPendingResponses: make(map[string]*utils.DataChannelRpcPendingResponseHandler),
		onClose:             make(map[string]func(types.LocalParticipant)),
		telemetryGuard:      &telemetry.ReferenceGuard{},
	}
	p.setupSignalling()

	p.id.Store(params.SID)
	p.dataChannelStats = telemetry.NewBytesTrackStats(
		p.params.Country,
		telemetry.BytesTrackIDForParticipantID(telemetry.BytesTrackTypeData, p.ID()),
		p.ID(),
		params.Telemetry,
		params.Reporter,
	)
	p.reliableDataInfo.lastPubReliableSeq.Store(params.LastPubReliableSeq)
	p.participantHelper.Store(params.ParticipantHelper)
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

	sessionTimer := observability.NewSessionTimer(p.params.SessionStartTime)
	params.Reporter.RegisterFunc(func(ts time.Time, tx roomobs.ParticipantSessionTx) bool {
		if dts := p.disconnectedAt.Load(); dts != nil {
			ts = *dts
			tx.ReportEndTime(ts)
		}

		millis, mins := sessionTimer.Advance(ts)
		tx.ReportDuration(uint16(millis))
		tx.ReportDurationMinutes(uint8(mins))

		return !p.IsDisconnected()
	})

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
	p.setupMetrics()

	return p, nil
}

func (p *ParticipantImpl) GetCountry() string {
	return p.params.Country
}

func (p *ParticipantImpl) GetTrailer() []byte {
	trailer := make([]byte, len(p.params.Trailer))
	copy(trailer, p.params.Trailer)
	return trailer
}

func (p *ParticipantImpl) GetLogger() logger.Logger {
	return p.params.Logger
}

func (p *ParticipantImpl) GetLoggerResolver() logger.DeferredFieldResolver {
	return p.params.LoggerResolver
}

func (p *ParticipantImpl) GetReporter() roomobs.ParticipantSessionReporter {
	return p.params.Reporter
}

func (p *ParticipantImpl) GetReporterResolver() roomobs.ParticipantReporterResolver {
	return p.params.ReporterResolver
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
	return p.id.Load().(livekit.ParticipantID)
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
	return utils.CloneProto(p.params.ClientConf)
}

func (p *ParticipantImpl) GetBufferFactory() *buffer.Factory {
	return p.params.Config.BufferFactory
}

// checkMetadataLimits check if name/metadata/attributes of a participant is within configured limits
func (p *ParticipantImpl) checkMetadataLimits(
	name string,
	metadata string,
	attributes map[string]string,
) error {
	if !p.params.LimitConfig.CheckParticipantNameLength(name) {
		return signalling.ErrNameExceedsLimits
	}

	if !p.params.LimitConfig.CheckMetadataSize(metadata) {
		return signalling.ErrMetadataExceedsLimits
	}

	if !p.params.LimitConfig.CheckAttributesSize(attributes) {
		return signalling.ErrAttributesExceedsLimits
	}

	return nil
}

func (p *ParticipantImpl) UpdateMetadata(update *livekit.UpdateParticipantMetadata, fromAdmin bool) error {
	lgr := p.params.Logger.WithUnlikelyValues(
		"update", logger.Proto(update),
		"fromAdmin", fromAdmin,
	)
	lgr.Debugw("updating participant metadata")

	var err error
	requestResponse := &livekit.RequestResponse{
		RequestId: update.RequestId,
	}
	sendRequestResponse := func() error {
		if !fromAdmin || (update.RequestId != 0 || err != nil) {
			requestResponse.Request = &livekit.RequestResponse_UpdateMetadata{
				UpdateMetadata: utils.CloneProto(update),
			}
			p.sendRequestResponse(requestResponse)
		}
		if err != nil {
			lgr.Warnw("could not update metadata", err)
		}
		return err
	}

	if !fromAdmin && !p.ClaimGrants().Video.GetCanUpdateOwnMetadata() {
		requestResponse.Reason = livekit.RequestResponse_NOT_ALLOWED
		requestResponse.Message = "does not have permission to update own metadata"
		err = signalling.ErrUpdateOwnMetadataNotAllowed
		return sendRequestResponse()
	}

	if err = p.checkMetadataLimits(update.Name, update.Metadata, update.Attributes); err != nil {
		switch err {
		case signalling.ErrNameExceedsLimits:
			requestResponse.Reason = livekit.RequestResponse_LIMIT_EXCEEDED
			requestResponse.Message = "exceeds name length limit"

		case signalling.ErrMetadataExceedsLimits:
			requestResponse.Reason = livekit.RequestResponse_LIMIT_EXCEEDED
			requestResponse.Message = "exceeds metadata size limit"

		case signalling.ErrAttributesExceedsLimits:
			requestResponse.Reason = livekit.RequestResponse_LIMIT_EXCEEDED
			requestResponse.Message = "exceeds attributes size limit"
		}
		return sendRequestResponse()
	}

	if update.Name != "" {
		p.SetName(update.Name)
	}
	if update.Metadata != "" {
		p.SetMetadata(update.Metadata)
	}
	if update.Attributes != nil {
		p.SetAttributes(update.Attributes)
	}
	return sendRequestResponse()
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
		Sid:              string(p.ID()),
		Identity:         string(p.params.Identity),
		Name:             grants.Name,
		State:            p.State(),
		JoinedAt:         p.ConnectedAt().Unix(),
		JoinedAtMs:       p.ConnectedAt().UnixMilli(),
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
			pi.Tracks = append(pi.Tracks, utils.CloneProto(pti.trackInfos[0]))
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

func (p *ParticipantImpl) TelemetryGuard() *telemetry.ReferenceGuard {
	return p.telemetryGuard
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

func (p *ParticipantImpl) OnStateChange(callback func(p types.LocalParticipant)) {
	p.lock.Lock()
	p.onStateChange = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnStateChange() func(p types.LocalParticipant) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onStateChange
}

func (p *ParticipantImpl) OnSubscriberReady(callback func(p types.LocalParticipant)) {
	p.lock.Lock()
	p.onSubscriberReady = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnSubscriberReady() func(p types.LocalParticipant) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onSubscriberReady
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

func (p *ParticipantImpl) OnDataMessage(callback func(types.LocalParticipant, []byte)) {
	p.lock.Lock()
	p.onDataMessage = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnDataMessage() func(types.LocalParticipant, []byte) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onDataMessage
}

func (p *ParticipantImpl) OnMetrics(callback func(types.Participant, *livekit.DataPacket)) {
	p.lock.Lock()
	p.onMetrics = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnMetrics() func(types.Participant, *livekit.DataPacket) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onMetrics
}

func (p *ParticipantImpl) OnUpdateSubscriptions(callback func(types.LocalParticipant, []livekit.TrackID, []*livekit.ParticipantTracks, bool)) {
	p.lock.Lock()
	p.onUpdateSubscriptions = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnUpdateSubscriptions() func(types.LocalParticipant, []livekit.TrackID, []*livekit.ParticipantTracks, bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onUpdateSubscriptions
}

func (p *ParticipantImpl) OnUpdateSubscriptionPermission(callback func(types.LocalParticipant, *livekit.SubscriptionPermission) error) {
	p.lock.Lock()
	p.onUpdateSubscriptionPermission = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnUpdateSubscriptionPermission() func(types.LocalParticipant, *livekit.SubscriptionPermission) error {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onUpdateSubscriptionPermission
}

func (p *ParticipantImpl) OnSyncState(callback func(types.LocalParticipant, *livekit.SyncState) error) {
	p.lock.Lock()
	p.onSyncState = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnSyncState() func(types.LocalParticipant, *livekit.SyncState) error {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onSyncState
}

func (p *ParticipantImpl) OnSimulateScenario(callback func(types.LocalParticipant, *livekit.SimulateScenario) error) {
	p.lock.Lock()
	p.onSimulateScenario = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnSimulateScenario() func(types.LocalParticipant, *livekit.SimulateScenario) error {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onSimulateScenario
}

func (p *ParticipantImpl) OnLeave(callback func(types.LocalParticipant, types.ParticipantCloseReason)) {
	p.lock.Lock()
	p.onLeave = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnLeave() func(types.LocalParticipant, types.ParticipantCloseReason) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.onLeave
}

func (p *ParticipantImpl) AddOnClose(key string, callback func(types.LocalParticipant)) {
	if p.isClosed.Load() {
		go callback(p)
		return
	}

	p.lock.Lock()
	if callback == nil {
		delete(p.onClose, key)
	} else {
		p.onClose[key] = callback
	}
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

func (p *ParticipantImpl) synthesizeAddTrackRequests(parsedOffer *sdp.SessionDescription) error {
	for _, m := range parsedOffer.MediaDescriptions {
		if !strings.EqualFold(m.MediaName.Media, "audio") && !strings.EqualFold(m.MediaName.Media, "video") {
			continue
		}

		cid := protosdp.GetMediaStreamTrack(m)
		if cid == "" {
			cid = guid.New(utils.TrackPrefix)
		}

		rids, ridsOk := protosdp.GetSimulcastRids(m)

		var (
			name        string
			trackSource livekit.TrackSource
			trackType   livekit.TrackType
		)
		if strings.EqualFold(m.MediaName.Media, "audio") {
			name = "synthesized-microphone"
			trackSource = livekit.TrackSource_MICROPHONE
			trackType = livekit.TrackType_AUDIO
		} else {
			name = "synthesized-camera"
			trackSource = livekit.TrackSource_CAMERA
			trackType = livekit.TrackType_VIDEO
		}
		req := &livekit.AddTrackRequest{
			Cid:        cid,
			Name:       name,
			Source:     trackSource,
			Type:       trackType,
			DisableDtx: true,
			Stereo:     false,
			Stream:     "camera",
		}
		if strings.EqualFold(m.MediaName.Media, "video") {
			if ridsOk {
				// add simulcast layers, NOTE: only quality can be set as dimensions/fps is not available
				n := min(len(rids), int(buffer.DefaultMaxLayerSpatial)+1)
				for i := 0; i < n; i++ {
					// WARN: casting int -> protobuf enum
					req.Layers = append(req.Layers, &livekit.VideoLayer{Quality: livekit.VideoQuality(i)})
				}
			} else {
				// dummy layer to ensure at least one layer is available
				req.Layers = []*livekit.VideoLayer{{}}
			}
		}
		p.AddTrack(req)
	}
	return nil
}

func (p *ParticipantImpl) updateRidsFromSDP(parsed *sdp.SessionDescription, unmatchVideos []*sdp.MediaDescription) {
	for _, m := range parsed.MediaDescriptions {
		if m.MediaName.Media != "video" || !slices.Contains(unmatchVideos, m) {
			continue
		}

		mst := protosdp.GetMediaStreamTrack(m)
		if mst == "" {
			continue
		}

		getRids := func(inRids buffer.VideoLayersRid) buffer.VideoLayersRid {
			var outRids buffer.VideoLayersRid
			rids, ok := protosdp.GetSimulcastRids(m)
			if ok {
				n := min(len(rids), len(inRids))
				for i := 0; i < n; i++ {
					outRids[i] = rids[i]
				}
				for i := n; i < len(inRids); i++ {
					outRids[i] = ""
				}
				outRids = buffer.NormalizeVideoLayersRid(outRids)
			} else {
				for i := 0; i < len(inRids); i++ {
					outRids[i] = ""
				}
			}

			return outRids
		}

		p.pendingTracksLock.Lock()
		pti := p.getPendingTrackPrimaryBySdpCid(mst)
		if pti != nil {
			pti.sdpRids = getRids(pti.sdpRids)
			p.pubLogger.Debugw(
				"pending track rids updated",
				"trackID", pti.trackInfos[0].Sid,
				"pendingTrack", pti,
			)

			ti := pti.trackInfos[0]
			for _, codec := range ti.Codecs {
				if codec.Cid == mst || codec.SdpCid == mst {
					mimeType := mime.NormalizeMimeType(codec.MimeType)
					for _, layer := range codec.Layers {
						layer.SpatialLayer = buffer.VideoQualityToSpatialLayer(mimeType, layer.Quality, ti)
						layer.Rid = buffer.VideoQualityToRid(mimeType, layer.Quality, ti, pti.sdpRids)
					}
				}
			}
		}
		p.pendingTracksLock.Unlock()

		if pti == nil {
			// track could already be published, but this could be back up codec offer,
			// so check in published tracks also
			mt := p.getPublishedTrackBySdpCid(mst)
			if mt != nil {
				mimeType := mt.(*MediaTrack).GetMimeTypeForSdpCid(mst)
				if mimeType != mime.MimeTypeUnknown {
					rids := getRids(buffer.DefaultVideoLayersRid)
					mt.(*MediaTrack).UpdateCodecRids(mimeType, rids)
					p.pubLogger.Debugw(
						"published track rids updated",
						"trackID", mt.ID(),
						"mime", mimeType,
						"track", logger.Proto(mt.ToProto()),
					)
				} else {
					p.pubLogger.Warnw(
						"could not get mime type for sdp cid", nil,
						"trackID", mt.ID(),
						"sdpCid", mst,
						"track", logger.Proto(mt.ToProto()),
					)
				}
			}
		}
	}
}

func (p *ParticipantImpl) HandleICETrickle(trickleRequest *livekit.TrickleRequest) {
	candidateInit, err := protosignalling.FromProtoTrickle(trickleRequest)
	if err != nil {
		p.params.Logger.Warnw("could not decode trickle", err)
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_UNCLASSIFIED_ERROR,
			Message: err.Error(),
			Request: &livekit.RequestResponse_Trickle{
				Trickle: utils.CloneProto(trickleRequest),
			},
		})
		return
	}

	p.TransportManager.AddICECandidate(candidateInit, trickleRequest.Target)
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(sd *livekit.SessionDescription) error {
	offer, offerId := protosignalling.FromProtoSessionDescription(sd)
	lgr := p.pubLogger.WithUnlikelyValues(
		"transport", livekit.SignalTarget_PUBLISHER,
		"offer", offer,
		"offerId", offerId,
	)

	lgr.Debugw("received offer")

	parsedOffer, err := offer.Unmarshal()
	if err != nil {
		lgr.Warnw("could not parse offer", err)
		return err
	}

	if p.params.UseOneShotSignallingMode {
		if err := p.synthesizeAddTrackRequests(parsedOffer); err != nil {
			lgr.Warnw("could not synthesize add track requests", err)
			return err
		}
	}

	err = p.TransportManager.HandleOffer(offer, offerId, p.MigrateState() == types.MigrateStateInit)
	if err != nil {
		lgr.Warnw("could not handle offer", err, "mungedOffer", offer)
		return err
	}

	if p.params.UseOneShotSignallingMode {
		if onSubscriberReady := p.getOnSubscriberReady(); onSubscriberReady != nil {
			go onSubscriberReady(p)
		}
	}

	p.handlePendingRemoteTracks()
	return nil
}

func (p *ParticipantImpl) onPublisherSetRemoteDescription() {
	offer := p.TransportManager.LastPublisherOfferPending()
	parsedOffer, err := offer.Unmarshal()
	if err != nil {
		p.pubLogger.Warnw("could not parse offer", err)
		return
	}

	// set publish codec preferences after remote description is set
	// and required transceivers are created
	unmatchAudios, unmatchVideos := p.populateSdpCid(parsedOffer)
	p.setCodecPreferencesForPublisher(parsedOffer, unmatchAudios, unmatchVideos)
	p.updateRidsFromSDP(parsedOffer, unmatchVideos)
}

func (p *ParticipantImpl) onPublisherAnswer(answer webrtc.SessionDescription, answerId uint32, midToTrackID map[string]string) error {
	if p.IsClosed() || p.IsDisconnected() {
		return nil
	}

	answer = p.configurePublisherAnswer(answer)
	p.pubLogger.Debugw(
		"sending answer",
		"transport", livekit.SignalTarget_PUBLISHER,
		"answer", answer,
		"answerId", answerId,
	)

	if p.params.UseSinglePeerConnection {
		return p.sendMappedSdpAnswer(answer, answerId, midToTrackID)
	}

	return p.sendSdpAnswer(answer, answerId)
}

func (p *ParticipantImpl) GetAnswer() (webrtc.SessionDescription, uint32, error) {
	if p.IsClosed() || p.IsDisconnected() {
		return webrtc.SessionDescription{}, 0, ErrParticipantSessionClosed
	}

	answer, answerId, err := p.TransportManager.GetAnswer()
	if err != nil {
		return answer, answerId, err
	}

	answer = p.configurePublisherAnswer(answer)
	p.pubLogger.Debugw(
		"returning answer",
		"transport", livekit.SignalTarget_PUBLISHER,
		"answer", answer,
		"answerId", answerId,
	)
	return answer, answerId, nil
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (p *ParticipantImpl) HandleAnswer(sd *livekit.SessionDescription) {
	answer, answerId := protosignalling.FromProtoSessionDescription(sd)
	p.subLogger.Debugw(
		"received answer",
		"transport", livekit.SignalTarget_SUBSCRIBER,
		"answer", answer,
		"answerId", answerId,
	)

	/* from server received join request to client answer
	 * 1. server send join response & offer
	 * ... swap candidates
	 * 2. client send answer
	 */
	signalConnCost := time.Since(p.ConnectedAt()).Milliseconds()
	p.TransportManager.UpdateSignalingRTT(uint32(signalConnCost))

	p.TransportManager.HandleAnswer(answer, answerId)
}

func (p *ParticipantImpl) handleMigrateTracks() []*MediaTrack {
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
			p.pubLogger.Warnw(
				"too many pending migrated tracks", nil,
				"trackID", pti.trackInfos[0].Sid,
				"count", len(pti.trackInfos),
				"cid", cid,
			)
		}

		mt := p.addMigratedTrack(cid, pti.trackInfos[0])
		if mt != nil {
			addedTracks = append(addedTracks, mt)
		} else {
			p.pubLogger.Warnw("could not find migrated track, migration failed", nil, "cid", cid)
			p.pendingTracksLock.Unlock()
			p.IssueFullReconnect(types.ParticipantCloseReasonMigrateCodecMismatch)
			return nil
		}
	}

	if len(addedTracks) != 0 {
		p.dirty.Store(true)
	}
	p.pendingTracksLock.Unlock()

	return addedTracks
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (p *ParticipantImpl) AddTrack(req *livekit.AddTrackRequest) {
	p.params.Logger.Debugw("add track request", "trackID", req.Cid, "request", logger.Proto(req))
	if !p.CanPublishSource(req.Source) {
		p.pubLogger.Warnw("no permission to publish track", nil, "trackID", req.Sid, "kind", req.Type)
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason: livekit.RequestResponse_NOT_ALLOWED,
			Request: &livekit.RequestResponse_AddTrack{
				AddTrack: utils.CloneProto(req),
			},
		})
		return
	}

	if req.Type != livekit.TrackType_AUDIO && req.Type != livekit.TrackType_VIDEO {
		p.pubLogger.Warnw("unsupported track type", nil, "trackID", req.Sid, "kind", req.Type)
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason: livekit.RequestResponse_UNSUPPORTED_TYPE,
			Request: &livekit.RequestResponse_AddTrack{
				AddTrack: utils.CloneProto(req),
			},
		})
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
	dataChannelReceiveState []*livekit.DataChannelReceiveState,
) {
	p.pendingTracksLock.Lock()
	for _, t := range mediaTracks {
		ti := t.GetTrack()

		if p.supervisor != nil {
			p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
			p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)
		}

		p.pendingTracks[t.GetCid()] = &pendingTrackInfo{
			trackInfos: []*livekit.TrackInfo{ti},
			migrated:   true,
			createdAt:  time.Now(),
		}
		p.pubLogger.Infow(
			"pending track added (migration)",
			"trackID", ti.Sid,
			"cid", t.GetCid(),
			"pendingTrack", p.pendingTracks[t.GetCid()],
		)
	}
	p.pendingTracksLock.Unlock()

	if len(mediaTracks) != 0 {
		p.setIsPublisher(true)
	}

	p.reliableDataInfo.joiningMessageLock.Lock()
	for _, state := range dataChannelReceiveState {
		p.reliableDataInfo.joiningMessageFirstSeqs[livekit.ParticipantID(state.PublisherSid)] = state.LastSeq + 1
	}
	p.reliableDataInfo.joiningMessageLock.Unlock()

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

	p.rpcLock.Lock()
	clear(p.rpcPendingAcks)
	for _, handler := range p.rpcPendingResponses {
		handler.Resolve("", utils.DataChannelRpcErrorFromBuiltInCodes(utils.DataChannelRpcRecipientDisconnected, ""))
	}
	p.rpcPendingResponses = make(map[string]*utils.DataChannelRpcPendingResponseHandler)
	p.rpcLock.Unlock()

	p.updateState(livekit.ParticipantInfo_DISCONNECTED)
	close(p.disconnected)

	// ensure this is synchronized
	p.CloseSignalConnection(types.SignallingCloseReasonParticipantClose)
	p.lock.RLock()
	onClose := maps.Values(p.onClose)
	p.lock.RUnlock()
	for _, cb := range onClose {
		cb(p)
	}

	// Close peer connections without blocking participant Close. If peer connections are gathering candidates
	// Close will block.
	go func() {
		p.SubscriptionManager.Close(isExpectedToResume)
		p.TransportManager.Close()

		p.metricsCollector.Stop()
		p.metricsReporter.Stop()
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
	if p.params.UseSinglePeerConnection {
		return
	}

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
	var migratedTracks []*MediaTrack
	if s == types.MigrateStateComplete {
		migratedTracks = p.handleMigrateTracks()
	}
	p.migrateState.Store(s)
	p.dirty.Store(true)

	switch s {
	case types.MigrateStateSync:
		p.TransportManager.ProcessPendingPublisherOffer()

	case types.MigrateStateComplete:
		if preState == types.MigrateStateSync {
			p.params.Logger.Infow("migration complete")

			if p.params.LastPubReliableSeq > 0 {
				p.reliableDataInfo.migrateInPubDataCache.Store(NewMigrationDataCache(p.params.LastPubReliableSeq, time.Now().Add(migrationWaitContinuousMsgDuration)))
			}
		}
		p.TransportManager.ProcessPendingPublisherDataChannels()
		go p.cacheForwarderState()
	}

	go func() {
		// launch callbacks in goroutine since they could block.
		// callbacks handle webhooks as well as db persistence
		for _, t := range migratedTracks {
			p.handleTrackPublished(t, true)
		}

		if s == types.MigrateStateComplete {
			// wait for all migrated track to be published,
			// it is possible that synthesized track publish above could
			// race with actual publish from client and the above synthesized
			// one could actually be a no-op because the actual publish path is active.
			//
			// if the actual publish path has not finished, the migration state change
			// callback could close the remote participant/tracks before the local track
			// is fully active.
			//
			// that could lead subscribers to unsubscribe due to source
			// track going away, i. e. in this case, the remote track close would have
			// notified the subscription manager, the subscription manager would
			// re-resolve to check if the track is still active and unsubscribe if none
			// is active, as local track is in the process of completing publish,
			// the check would have resolved to an empty track leading to unsubscription.
			go func() {
				startTime := time.Now()
				for {
					if !p.hasPendingMigratedTrack() || p.IsDisconnected() || time.Since(startTime) > 15*time.Second {
						// a time out just to be safe, but it should not be needed
						p.migratedTracksPublishedFuse.Break()
						return
					}

					time.Sleep(20 * time.Millisecond)
				}
			}()

			<-p.migratedTracksPublishedFuse.Watch()
		}

		if onMigrateStateChange := p.getOnMigrateStateChange(); onMigrateStateChange != nil {
			onMigrateStateChange(p, s)
		}
	}()
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
	minQuality := livekit.ConnectionQuality_EXCELLENT
	minScore := connectionquality.MaxMOS

	for _, pt := range p.GetPublishedTracks() {
		score, quality := pt.(types.LocalMediaTrack).GetConnectionScoreAndQuality()
		if utils.IsConnectionQualityLower(minQuality, quality) {
			minQuality = quality
			minScore = score
		} else if quality == minQuality && score < minScore {
			minScore = score
		}
	}

	subscribedTracks := p.SubscriptionManager.GetSubscribedTracks()
	for _, subTrack := range subscribedTracks {
		score, quality := subTrack.DownTrack().GetConnectionScoreAndQuality()
		if utils.IsConnectionQualityLower(minQuality, quality) {
			minQuality = quality
			minScore = score
		} else if quality == minQuality && score < minScore {
			minScore = score
		}
	}

	prometheus.RecordQuality(minQuality, minScore)

	if minQuality == livekit.ConnectionQuality_LOST && !p.ProtocolVersion().SupportsConnectionQualityLost() {
		minQuality = livekit.ConnectionQuality_POOR
	}

	p.lock.Lock()
	if minQuality != p.connectionQuality {
		p.params.Logger.Debugw("connection quality changed", "from", p.connectionQuality, "to", minQuality)
	}
	p.connectionQuality = minQuality
	p.lock.Unlock()

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

	if info := p.helper().GetParticipantInfo(pID); info != nil {
		_ = p.SendParticipantUpdate([]*livekit.ParticipantInfo{info})
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

var _ transport.Handler = (*AnyTransportHandler)(nil)

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

func (h PublisherTransportHandler) OnSetRemoteDescriptionOffer() {
	h.p.onPublisherSetRemoteDescription()
}

func (h PublisherTransportHandler) OnAnswer(sd webrtc.SessionDescription, answerId uint32, midToTrackID map[string]string) error {
	return h.p.onPublisherAnswer(sd, answerId, midToTrackID)
}

func (h PublisherTransportHandler) OnTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	h.p.onMediaTrack(track, rtpReceiver)
}

func (h PublisherTransportHandler) OnInitialConnected() {
	h.p.onPublisherInitialConnected()
}

func (h PublisherTransportHandler) OnDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	h.p.onReceivedDataMessage(kind, data)
}

func (h PublisherTransportHandler) OnDataMessageUnlabeled(data []byte) {
	h.p.onReceivedDataMessageUnlabeled(data)
}

func (h PublisherTransportHandler) OnDataSendError(err error) {
	h.p.onDataSendError(err)
}

func (h PublisherTransportHandler) OnUnmatchedMedia(numAudios uint32, numVideos uint32) error {
	return h.p.sendMediaSectionsRequirement(numAudios, numVideos)
}

// ----------------------------------------------------------

type SubscriberTransportHandler struct {
	AnyTransportHandler
}

func (h SubscriberTransportHandler) OnOffer(sd webrtc.SessionDescription, offerId uint32) error {
	return h.p.onSubscriberOffer(sd, offerId)
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

func (p *ParticipantImpl) setupSignalling() {
	p.signalling = signalling.NewSignalling(signalling.SignallingParams{
		Logger: p.params.Logger,
	})
	p.signalHandler = signalling.NewSignalHandler(signalling.SignalHandlerParams{
		Logger:      p.params.Logger,
		Participant: p,
	})
	p.signaller = signalling.NewSignallerAsync(signalling.SignallerAsyncParams{
		Logger:      p.params.Logger,
		Participant: p,
	})
}

func (p *ParticipantImpl) setupTransportManager() error {
	p.twcc = twcc.NewTransportWideCCResponder()
	p.twcc.OnFeedback(func(pkts []rtcp.Packet) {
		p.postRtcp(pkts)
	})
	ath := AnyTransportHandler{p: p}
	var pth transport.Handler = PublisherTransportHandler{ath}
	var sth transport.Handler = SubscriberTransportHandler{ath}

	subscriberAsPrimary := !p.params.UseOneShotSignallingMode && (p.ProtocolVersion().SubscriberAsPrimary() && p.CanSubscribe()) && !p.params.UseSinglePeerConnection
	if subscriberAsPrimary {
		sth = PrimaryTransportHandler{sth, p}
	} else {
		pth = PrimaryTransportHandler{pth, p}
	}

	params := TransportManagerParams{
		// primary connection does not change, canSubscribe can change if permission was updated
		// after the participant has joined
		SubscriberAsPrimary:          subscriberAsPrimary,
		UseSinglePeerConnection:      p.params.UseSinglePeerConnection,
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
		DatachannelSlowThreshold:     p.params.DatachannelSlowThreshold,
		Logger:                       p.params.Logger.WithComponent(sutils.ComponentTransport),
		PublisherHandler:             pth,
		SubscriberHandler:            sth,
		DataChannelStats:             p.dataChannelStats,
		UseOneShotSignallingMode:     p.params.UseOneShotSignallingMode,
		FireOnTrackBySdp:             p.params.FireOnTrackBySdp,
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
		Participant: p,
		Logger:      p.subLogger.WithoutSampler(),
		TrackResolver: func(lp types.LocalParticipant, ti livekit.TrackID) types.MediaResolverResult {
			return p.helper().ResolveMediaTrack(lp, ti)
		},
		Telemetry:                p.params.Telemetry,
		OnTrackSubscribed:        p.onTrackSubscribed,
		OnTrackUnsubscribed:      p.onTrackUnsubscribed,
		OnSubscriptionError:      p.onSubscriptionError,
		SubscriptionLimitVideo:   p.params.SubscriptionLimitVideo,
		SubscriptionLimitAudio:   p.params.SubscriptionLimitAudio,
		UseOneShotSignallingMode: p.params.UseOneShotSignallingMode,
	})
}

func (p *ParticipantImpl) MetricsCollectorTimeToCollectMetrics() {
	publisherRTT, ok := p.TransportManager.GetPublisherRTT()
	if ok {
		p.metricsCollector.AddPublisherRTT(p.Identity(), float32(publisherRTT))
	}

	subscriberRTT, ok := p.TransportManager.GetSubscriberRTT()
	if ok {
		p.metricsCollector.AddSubscriberRTT(float32(subscriberRTT))
	}
}

func (p *ParticipantImpl) MetricsCollectorBatchReady(mb *livekit.MetricsBatch) {
	if onMetrics := p.getOnMetrics(); onMetrics != nil {
		onMetrics(p, &livekit.DataPacket{
			ParticipantIdentity: string(p.Identity()),
			Value: &livekit.DataPacket_Metrics{
				Metrics: mb,
			},
		})
	}
}

func (p *ParticipantImpl) MetricsReporterBatchReady(mb *livekit.MetricsBatch) {
	dpData, err := proto.Marshal(&livekit.DataPacket{
		ParticipantIdentity: string(p.Identity()),
		Value: &livekit.DataPacket_Metrics{
			Metrics: mb,
		},
	})
	if err != nil {
		p.params.Logger.Errorw("failed to marshal data packet", err)
		return
	}

	p.TransportManager.SendDataMessage(livekit.DataPacket_RELIABLE, dpData)
}

func (p *ParticipantImpl) setupMetrics() {
	if !p.params.EnableMetrics {
		return
	}

	p.metricTimestamper = metric.NewMetricTimestamper(metric.MetricTimestamperParams{
		Config: p.params.MetricConfig.Timestamper,
		Logger: p.params.Logger,
	})
	p.metricsCollector = metric.NewMetricsCollector(metric.MetricsCollectorParams{
		ParticipantIdentity: p.Identity(),
		Config:              p.params.MetricConfig.Collector,
		Provider:            p,
		Logger:              p.params.Logger,
	})
	p.metricsReporter = metric.NewMetricsReporter(metric.MetricsReporterParams{
		ParticipantIdentity: p.Identity(),
		Config:              p.params.MetricConfig.Reporter,
		Consumer:            p,
		Logger:              p.params.Logger,
	})
}

func (p *ParticipantImpl) updateState(state livekit.ParticipantInfo_State) {
	var oldState livekit.ParticipantInfo_State
	for {
		oldState = p.state.Load().(livekit.ParticipantInfo_State)
		if state <= oldState {
			p.params.Logger.Debugw("ignoring out of order participant state", "state", state.String())
			return
		}
		if state == livekit.ParticipantInfo_ACTIVE {
			p.lastActiveAt.CompareAndSwap(nil, pointer.To(time.Now()))
		}
		if p.state.CompareAndSwap(oldState, state) {
			break
		}
	}

	p.params.Logger.Debugw("updating participant state", "state", state.String())
	p.dirty.Store(true)

	if onStateChange := p.getOnStateChange(); onStateChange != nil {
		go onStateChange(p)
	}

	if state == livekit.ParticipantInfo_DISCONNECTED && oldState == livekit.ParticipantInfo_ACTIVE {
		p.disconnectedAt.Store(pointer.To(time.Now()))
		prometheus.RecordSessionDuration(int(p.ProtocolVersion()), time.Since(*p.lastActiveAt.Load()))
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
func (p *ParticipantImpl) onSubscriberOffer(offer webrtc.SessionDescription, offerId uint32) error {
	p.subLogger.Debugw(
		"sending offer",
		"transport", livekit.SignalTarget_SUBSCRIBER,
		"offer", offer,
		"offerId", offerId,
	)
	return p.sendSdpOffer(offer, offerId)
}

func (p *ParticipantImpl) removePublishedTrack(track types.MediaTrack) {
	p.RemovePublishedTrack(track, false)
	if p.ProtocolVersion().SupportsUnpublish() {
		p.sendTrackUnpublished(track.ID())
	} else {
		// for older clients that don't support unpublish, mute to avoid them sending data
		p.sendTrackMuted(track.ID(), true)
	}
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(rtcTrack *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	if p.IsDisconnected() {
		return
	}

	var codec webrtc.RTPCodecParameters
	var fromSdp bool
	if rtcTrack.Kind() == webrtc.RTPCodecTypeVideo && p.params.ClientInfo.FireTrackByRTPPacket() {
		if rtcTrack.Codec().PayloadType == 0 {
			go func() {
				// wait for the first packet to determine the codec
				bytes := make([]byte, 1500)
				_, _, err := rtcTrack.Read(bytes)
				if err != nil {
					if !errors.Is(err, io.EOF) {
						p.params.Logger.Warnw(
							"could not read first packet to determine codec, track will be ignored", err,
							"trackID", rtcTrack.ID(),
							"StreamID", rtcTrack.StreamID(),
						)
					}
					return
				}
				p.onMediaTrack(rtcTrack, rtpReceiver)
			}()
			return
		}
		codec = rtcTrack.Codec()
	} else {
		// track fired by sdp
		codecs := rtpReceiver.GetParameters().Codecs
		if len(codecs) == 0 {
			p.pubLogger.Errorw(
				"no negotiated codecs for track, track will be ignored", nil,
				"trackID", rtcTrack.ID(),
				"StreamID", rtcTrack.StreamID(),
			)
			return
		}
		codec = codecs[0]
		fromSdp = true
	}
	p.params.Logger.Debugw(
		"onMediaTrack",
		"codec", codec,
		"payloadType", codec.PayloadType,
		"fromSdp", fromSdp,
		"parameters", rtpReceiver.GetParameters(),
	)

	var track sfu.TrackRemote = sfu.NewTrackRemoteFromSdp(rtcTrack, codec)
	publishedTrack, isNewTrack, isReceiverAdded, sdpRids := p.mediaTrackReceived(track, rtpReceiver)
	if publishedTrack == nil {
		p.pubLogger.Debugw(
			"webrtc track published but can't find MediaTrack in pendingTracks",
			"kind", track.Kind().String(),
			"webrtcTrackID", track.ID(),
			"rid", track.RID(),
			"SSRC", track.SSRC(),
			"mime", mime.NormalizeMimeType(codec.MimeType),
			"isReceiverAdded", isReceiverAdded,
			"sdpRids", logger.StringSlice(sdpRids[:]),
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

	p.pubLogger.Infow(
		"mediaTrack published",
		"kind", track.Kind().String(),
		"trackID", publishedTrack.ID(),
		"webrtcTrackID", track.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC(),
		"mime", mime.NormalizeMimeType(codec.MimeType),
		"trackInfo", logger.Proto(publishedTrack.ToProto()),
		"fromSdp", fromSdp,
		"isReceiverAdded", isReceiverAdded,
		"sdpRids", logger.StringSlice(sdpRids[:]),
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

func (p *ParticipantImpl) onReceivedDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	if p.IsDisconnected() || !p.CanPublishData() {
		return
	}

	p.dataChannelStats.AddBytes(uint64(len(data)), false)

	dp := &livekit.DataPacket{}
	if err := proto.Unmarshal(data, dp); err != nil {
		p.pubLogger.Warnw("could not parse data packet", err)
		return
	}

	dp.ParticipantSid = string(p.ID())
	if kind == livekit.DataPacket_RELIABLE && dp.Sequence > 0 {
		if p.reliableDataInfo.stopReliableByMigrateOut.Load() {
			return
		}

		if migrationCache := p.reliableDataInfo.migrateInPubDataCache.Load(); migrationCache != nil {
			switch migrationCache.Add(dp) {
			case MigrationDataCacheStateWaiting:
				// waiting for the reliable sequence to continue from last node
				return

			case MigrationDataCacheStateTimeout:
				p.reliableDataInfo.migrateInPubDataCache.Store(nil)
				// waiting time out, handle all cached messages
				cachedMsgs := migrationCache.Get()
				if len(cachedMsgs) == 0 {
					p.pubLogger.Warnw(
						"migration data cache timed out, no cached messages received", nil,
						"lastPubReliableSeq", p.params.LastPubReliableSeq,
					)
				} else {
					p.pubLogger.Warnw(
						"migration data cache timed out, handling cached messages", nil,
						"cachedFirstSeq", cachedMsgs[0].Sequence,
						"cachedLastSeq", cachedMsgs[len(cachedMsgs)-1].Sequence,
						"lastPubReliableSeq", p.params.LastPubReliableSeq,
					)
				}
				for _, cachedDp := range cachedMsgs {
					p.handleReceivedDataMessage(kind, cachedDp)
				}
				return

			case MigrationDataCacheStateDone:
				// see the continous message, drop the cache
				p.reliableDataInfo.migrateInPubDataCache.Store(nil)
			}
		}
	}

	p.handleReceivedDataMessage(kind, dp)
}

func (p *ParticipantImpl) handleReceivedDataMessage(kind livekit.DataPacket_Kind, dp *livekit.DataPacket) {
	if kind == livekit.DataPacket_RELIABLE && dp.Sequence > 0 {
		if p.reliableDataInfo.lastPubReliableSeq.Load() >= dp.Sequence {
			p.params.Logger.Infow(
				"received out of order reliable data packet",
				"lastPubReliableSeq", p.reliableDataInfo.lastPubReliableSeq.Load(),
				"dpSequence", dp.Sequence,
			)
			return
		}

		p.reliableDataInfo.lastPubReliableSeq.Store(dp.Sequence)
	}

	// trust the channel that it came in as the source of truth
	dp.Kind = kind

	shouldForwardData := true
	shouldForwardMetrics := false
	overrideSenderIdentity := true
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
			u.ParticipantSid = string(p.ID())
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
		p.metricTimestamper.Process(payload.Metrics)
	case *livekit.DataPacket_RpcRequest:
		if payload.RpcRequest == nil {
			return
		}
		p.pubLogger.Infow(
			"received RPC request",
			"method", payload.RpcRequest.Method,
			"rpc_request_id", payload.RpcRequest.Id,
			"destinationIdentities", dp.DestinationIdentities,
		)
	case *livekit.DataPacket_RpcResponse:
		if payload.RpcResponse == nil {
			return
		}
		p.pubLogger.Infow(
			"received RPC response",
			"rpc_request_id", payload.RpcResponse.RequestId,
		)

		rpcResponse := payload.RpcResponse
		switch res := rpcResponse.Value.(type) {
		case *livekit.RpcResponse_Payload:
			shouldForwardData = !p.handleIncomingRpcResponse(payload.RpcResponse.GetRequestId(), res.Payload, nil)
		case *livekit.RpcResponse_Error:
			shouldForwardData = !p.handleIncomingRpcResponse(payload.RpcResponse.GetRequestId(), "", &utils.DataChannelRpcError{
				Code:    utils.DataChannelRpcErrorCode(res.Error.GetCode()),
				Message: res.Error.GetMessage(),
				Data:    res.Error.GetData(),
			})
		}
	case *livekit.DataPacket_RpcAck:
		if payload.RpcAck == nil {
			return
		}
		p.pubLogger.Infow(
			"received RPC ack",
			"rpc_request_id", payload.RpcAck.RequestId,
		)

		shouldForwardData = !p.handleIncomingRpcAck(payload.RpcAck.GetRequestId())
	case *livekit.DataPacket_StreamHeader:
		if payload.StreamHeader == nil {
			return
		}

		prometheus.RecordDataPacketStream(payload.StreamHeader, len(dp.DestinationIdentities))

		if p.IsAgent() && dp.ParticipantIdentity != "" && string(p.params.Identity) != dp.ParticipantIdentity {
			switch contentHeader := payload.StreamHeader.ContentHeader.(type) {
			case *livekit.DataStream_Header_TextHeader:
				contentHeader.TextHeader.Generated = true
				overrideSenderIdentity = false
			default:
				overrideSenderIdentity = true
			}
		}
	case *livekit.DataPacket_StreamChunk:
		if payload.StreamChunk == nil {
			return
		}
	case *livekit.DataPacket_StreamTrailer:
		if payload.StreamTrailer == nil {
			return
		}
	case *livekit.DataPacket_EncryptedPacket:
		if payload.EncryptedPacket == nil {
			return
		}
	default:
		p.pubLogger.Warnw("received unsupported data packet", nil, "payload", payload)
	}

	// SFU typically asserts the sender's identity. However, agents are able to
	// publish data on behalf of the participant in case of transcriptions/text streams
	// in those cases we'd leave the existing identity on the data packet alone.
	if overrideSenderIdentity {
		if p.Hidden() {
			dp.ParticipantIdentity = ""
		} else {
			dp.ParticipantIdentity = string(p.params.Identity)
		}
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
}

func (p *ParticipantImpl) onReceivedDataMessageUnlabeled(data []byte) {
	if p.IsDisconnected() || !p.CanPublishData() {
		return
	}

	p.dataChannelStats.AddBytes(uint64(len(data)), false)

	if onDataMessage := p.getOnDataMessage(); onDataMessage != nil {
		onDataMessage(p, data)
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

	if p.params.UseOneShotSignallingMode || p.params.UseSinglePeerConnection {
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

	if !p.sessionStartRecorded.Swap(true) {
		prometheus.RecordSessionStartTime(int(p.ProtocolVersion()), time.Since(p.params.SessionStartTime))
	}
	p.updateState(livekit.ParticipantInfo_ACTIVE)
}

func (p *ParticipantImpl) onPrimaryTransportFullyEstablished() {
	p.replayJoiningReliableMessages()
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

	return p.sendStreamStateUpdate(streamStateUpdate)
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
		for _, layer := range buffer.GetVideoLayersForMimeType(maxSubscribedQuality.CodecMime, trackInfo) {
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
		subscribedQuality.Codec = strings.ToLower(strings.TrimPrefix(subscribedQuality.Codec, mime.MimeTypePrefixVideo))
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
	return p.sendSubscribedQualityUpdate(subscribedQualityUpdate)
}

func (p *ParticipantImpl) onSubscribedAudioCodecChange(
	trackID livekit.TrackID,
	codecs []*livekit.SubscribedAudioCodec,
) error {
	if p.params.DisableDynacast {
		return nil
	}

	if len(codecs) == 0 {
		return nil
	}

	// normalize the codec name
	for _, codec := range codecs {
		codec.Codec = strings.ToLower(strings.TrimPrefix(codec.Codec, mime.MimeTypePrefixAudio))
	}

	subscribedAudioCodecUpdate := &livekit.SubscribedAudioCodecUpdate{
		TrackSid:              string(trackID),
		SubscribedAudioCodecs: codecs,
	}
	p.pubLogger.Debugw(
		"sending subscribed audio codec update",
		"trackID", trackID,
		"update", logger.Proto(subscribedAudioCodecUpdate),
	)
	return p.sendSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate)
}

func (p *ParticipantImpl) addPendingTrackLocked(req *livekit.AddTrackRequest) *livekit.TrackInfo {
	if req.Sid != "" {
		track := p.GetPublishedTrack(livekit.TrackID(req.Sid))
		if track == nil {
			p.pubLogger.Infow("could not find existing track for multi-codec simulcast", "trackID", req.Sid)
			p.sendRequestResponse(&livekit.RequestResponse{
				Reason: livekit.RequestResponse_NOT_FOUND,
				Request: &livekit.RequestResponse_AddTrack{
					AddTrack: utils.CloneProto(req),
				},
			})
			return nil
		}

		track.(*MediaTrack).UpdateCodecInfo(req.SimulcastCodecs)
		return track.ToProto()
	}

	backupCodecPolicy := req.BackupCodecPolicy
	if backupCodecPolicy != livekit.BackupCodecPolicy_SIMULCAST && p.params.DisableCodecRegression {
		backupCodecPolicy = livekit.BackupCodecPolicy_SIMULCAST
	}

	cloneLayers := func(layers []*livekit.VideoLayer) []*livekit.VideoLayer {
		if len(layers) == 0 {
			return nil
		}

		clonedLayers := make([]*livekit.VideoLayer, 0, len(layers))
		for _, l := range layers {
			clonedLayers = append(clonedLayers, utils.CloneProto(l))
		}
		slices.SortFunc(clonedLayers, func(i, j *livekit.VideoLayer) int {
			return int(i.Quality) - int(j.Quality)
		})
		return clonedLayers
	}

	ti := &livekit.TrackInfo{
		Type:              req.Type,
		Name:              req.Name,
		Width:             req.Width,
		Height:            req.Height,
		Muted:             req.Muted,
		DisableDtx:        req.DisableDtx,
		Source:            req.Source,
		Layers:            cloneLayers(req.Layers),
		DisableRed:        req.DisableRed,
		Stereo:            req.Stereo,
		Encryption:        req.Encryption,
		Stream:            req.Stream,
		BackupCodecPolicy: backupCodecPolicy,
		AudioFeatures:     sutils.DedupeSlice(req.AudioFeatures),
	}
	if req.Stereo && !slices.Contains(ti.AudioFeatures, livekit.AudioTrackFeature_TF_STEREO) {
		ti.AudioFeatures = append(ti.AudioFeatures, livekit.AudioTrackFeature_TF_STEREO)
	}
	if req.DisableDtx && !slices.Contains(ti.AudioFeatures, livekit.AudioTrackFeature_TF_NO_DTX) {
		ti.AudioFeatures = append(ti.AudioFeatures, livekit.AudioTrackFeature_TF_NO_DTX)
	}
	if ti.Stream == "" {
		ti.Stream = StreamFromTrackSource(ti.Source)
	}
	p.setTrackID(req.Cid, ti)

	if len(req.SimulcastCodecs) == 0 {
		// clients not supporting simulcast codecs, synthesise a codec
		videoLayerMode := livekit.VideoLayer_MODE_UNUSED
		if p.params.ClientInfo.isOBS() {
			videoLayerMode = livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR
		}
		ti.Codecs = append(ti.Codecs, &livekit.SimulcastCodecInfo{
			Cid:            req.Cid,
			Layers:         cloneLayers(req.Layers),
			VideoLayerMode: videoLayerMode,
		})
	} else {
		seenCodecs := make(map[string]struct{})
		for _, codec := range req.SimulcastCodecs {
			if codec.Codec == "" {
				p.pubLogger.Warnw(
					"simulcast codec without mime type", nil,
					"trackID", ti.Sid,
					"track", logger.Proto(ti),
					"addTrackRequest", logger.Proto(req),
				)
			}

			mimeType := codec.Codec
			videoLayerMode := codec.VideoLayerMode
			if req.Type == livekit.TrackType_VIDEO {
				if !mime.IsMimeTypeStringVideo(mimeType) {
					mimeType = mime.MimeTypePrefixVideo + mimeType
				}
				if !IsCodecEnabled(p.enabledPublishCodecs, webrtc.RTPCodecCapability{MimeType: mimeType}) {
					altCodec := selectAlternativeVideoCodec(p.enabledPublishCodecs)
					p.pubLogger.Infow(
						"falling back to alternative video codec",
						"codec", mimeType,
						"altCodec", altCodec,
						"trackID", ti.Sid,
					)
					// select an alternative MIME type that's generally supported
					mimeType = altCodec
				}
				if videoLayerMode == livekit.VideoLayer_MODE_UNUSED {
					if mime.IsMimeTypeStringSVCCapable(mimeType) {
						videoLayerMode = livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM
					} else {
						if p.params.ClientInfo.isOBS() {
							videoLayerMode = livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR
						} else {
							videoLayerMode = livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM
						}
					}
				}
			} else if req.Type == livekit.TrackType_AUDIO {
				if !mime.IsMimeTypeStringAudio(mimeType) {
					mimeType = mime.MimeTypePrefixAudio + mimeType
				}
				if !IsCodecEnabled(p.enabledPublishCodecs, webrtc.RTPCodecCapability{MimeType: mimeType}) {
					altCodec := selectAlternativeAudioCodec(p.enabledPublishCodecs)
					p.pubLogger.Infow(
						"falling back to alternative audio codec",
						"codec", mimeType,
						"altCodec", altCodec,
						"trackID", ti.Sid,
					)
					// select an alternative MIME type that's generally supported
					mimeType = altCodec
				}
			}

			if _, ok := seenCodecs[mimeType]; ok || mimeType == "" {
				continue
			}
			seenCodecs[mimeType] = struct{}{}

			ti.Codecs = append(ti.Codecs, &livekit.SimulcastCodecInfo{
				MimeType:       mimeType,
				Cid:            codec.Cid,
				VideoLayerMode: videoLayerMode,
			})
		}

		// set up layers with codec specific layers,
		// fall back to common layers if codec specific layer is not available
		for idx, codec := range ti.Codecs {
			found := false
			for _, simulcastCodec := range req.SimulcastCodecs {
				if mime.GetMimeTypeCodec(codec.MimeType) != mime.NormalizeMimeTypeCodec(simulcastCodec.Codec) {
					continue
				}

				if len(simulcastCodec.Layers) != 0 {
					codec.Layers = cloneLayers(simulcastCodec.Layers)
				} else {
					codec.Layers = cloneLayers(req.Layers)
				}
				found = true
				break
			}

			if !found {
				// could happen if an alternate codec is selected and that is not in the simulcast codecs list
				codec.Layers = cloneLayers(req.Layers)
			}

			// populate simulcast flag for compatibility, true if primary codec is not SVC and has multiple layers
			if idx == 0 && codec.VideoLayerMode != livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM && len(codec.Layers) > 1 {
				ti.Simulcast = true
			}
		}
	}

	p.params.Telemetry.TrackPublishRequested(context.Background(), p.ID(), p.Identity(), utils.CloneProto(ti))

	if p.supervisor != nil {
		p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
		p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)
	}

	if p.getPublishedTrackBySignalCid(req.Cid) != nil || p.getPublishedTrackBySdpCid(req.Cid) != nil || p.pendingTracks[req.Cid] != nil {
		if p.pendingTracks[req.Cid] == nil {
			pti := &pendingTrackInfo{
				trackInfos: []*livekit.TrackInfo{ti},
				createdAt:  time.Now(),
				queued:     true,
			}
			if ti.Type == livekit.TrackType_VIDEO {
				pti.sdpRids = buffer.DefaultVideoLayersRid // could get updated from SDP
			}
			p.pendingTracks[req.Cid] = pti
		} else {
			p.pendingTracks[req.Cid].trackInfos = append(p.pendingTracks[req.Cid].trackInfos, ti)
		}
		p.pubLogger.Infow(
			"pending track queued",
			"trackID", ti.Sid,
			"request", logger.Proto(req),
			"pendingTrack", p.pendingTracks[req.Cid],
		)
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason: livekit.RequestResponse_QUEUED,
			Request: &livekit.RequestResponse_AddTrack{
				AddTrack: utils.CloneProto(req),
			},
		})
		return nil
	}

	pti := &pendingTrackInfo{
		trackInfos: []*livekit.TrackInfo{ti},
		createdAt:  time.Now(),
	}
	if ti.Type == livekit.TrackType_VIDEO {
		pti.sdpRids = buffer.DefaultVideoLayersRid // could get updated from SDP
	}
	p.pendingTracks[req.Cid] = pti
	p.pubLogger.Debugw(
		"pending track added",
		"trackID", ti.Sid,
		"request", logger.Proto(req),
		"pendingTrack", p.pendingTracks[req.Cid],
	)
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

func (p *ParticipantImpl) SetTrackMuted(mute *livekit.MuteTrackRequest, fromAdmin bool) *livekit.TrackInfo {
	// when request is coming from admin, send message to current participant
	if fromAdmin {
		p.sendTrackMuted(livekit.TrackID(mute.Sid), mute.Muted)
	}

	return p.setTrackMuted(mute, fromAdmin)
}

func (p *ParticipantImpl) setTrackMuted(mute *livekit.MuteTrackRequest, fromAdmin bool) *livekit.TrackInfo {
	trackID := livekit.TrackID(mute.Sid)
	p.dirty.Store(true)
	if p.supervisor != nil {
		p.supervisor.SetPublicationMute(trackID, mute.Muted)
	}

	track, changed := p.UpTrackManager.SetPublishedTrackMuted(trackID, mute.Muted)
	var trackInfo *livekit.TrackInfo
	if track != nil {
		trackInfo = track.ToProto()
	}

	// update mute status in any pending/queued add track requests too
	p.pendingTracksLock.RLock()
	for _, pti := range p.pendingTracks {
		for i, ti := range pti.trackInfos {
			if livekit.TrackID(ti.Sid) == trackID {
				ti = utils.CloneProto(ti)
				changed = changed || ti.Muted != mute.Muted
				ti.Muted = mute.Muted
				pti.trackInfos[i] = ti
				if trackInfo == nil {
					trackInfo = ti
				}
			}
		}
	}
	p.pendingTracksLock.RUnlock()

	if trackInfo != nil && changed {
		if mute.Muted {
			p.params.Telemetry.TrackMuted(context.Background(), p.ID(), trackInfo)
		} else {
			p.params.Telemetry.TrackUnmuted(context.Background(), p.ID(), trackInfo)
		}
	}

	if trackInfo == nil && !fromAdmin {
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason: livekit.RequestResponse_NOT_FOUND,
			Request: &livekit.RequestResponse_Mute{
				Mute: utils.CloneProto(mute),
			},
		})
	}

	return trackInfo
}

func (p *ParticipantImpl) mediaTrackReceived(track sfu.TrackRemote, rtpReceiver *webrtc.RTPReceiver) (
	*MediaTrack,
	bool,
	bool,
	buffer.VideoLayersRid,
) {
	p.pendingTracksLock.Lock()
	newTrack := false

	mid := p.TransportManager.GetPublisherMid(rtpReceiver)
	p.pubLogger.Debugw(
		"media track received",
		"kind", track.Kind().String(),
		"trackID", track.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC(),
		"mime", mime.NormalizeMimeType(track.Codec().MimeType),
		"mid", mid,
	)
	if mid == "" {
		p.pendingRemoteTracks = append(
			p.pendingRemoteTracks,
			&pendingRemoteTrack{track: track.RTCTrack(), receiver: rtpReceiver},
		)
		p.pendingTracksLock.Unlock()
		p.pubLogger.Warnw("could not get mid for track", nil, "trackID", track.ID())
		return nil, false, false, buffer.VideoLayersRid{}
	}

	// use existing media track to handle simulcast
	var pubTime time.Duration
	var isMigrated bool
	var ridsFromSdp buffer.VideoLayersRid
	mt, ok := p.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti, sdpRids, migrated, createdAt := p.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()), true)
		ridsFromSdp = sdpRids
		if ti == nil {
			p.pendingRemoteTracks = append(
				p.pendingRemoteTracks,
				&pendingRemoteTrack{track: track.RTCTrack(), receiver: rtpReceiver},
			)
			p.pendingTracksLock.Unlock()
			return nil, false, false, ridsFromSdp
		}
		isMigrated = migrated

		// check if the migrated track has correct codec
		if migrated && len(ti.Codecs) > 0 {
			parameters := rtpReceiver.GetParameters()
			var codecFound int
			for _, c := range ti.Codecs {
				for _, nc := range parameters.Codecs {
					if mime.IsMimeTypeStringEqual(nc.MimeType, c.MimeType) {
						codecFound++
						break
					}
				}
			}
			if codecFound != len(ti.Codecs) {
				p.pubLogger.Warnw("migrated track codec mismatched", nil, "track", logger.Proto(ti), "webrtcCodec", parameters)
				p.pendingTracksLock.Unlock()
				p.IssueFullReconnect(types.ParticipantCloseReasonMigrateCodecMismatch)
				return nil, false, false, ridsFromSdp
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

		mimeType := mime.NormalizeMimeType(ti.MimeType)
		for _, layer := range ti.Layers {
			layer.SpatialLayer = buffer.VideoQualityToSpatialLayer(mimeType, layer.Quality, ti)
			layer.Rid = buffer.VideoQualityToRid(mimeType, layer.Quality, ti, sdpRids)
		}

		mt = p.addMediaTrack(signalCid, ti)
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

	_, isReceiverAdded := mt.AddReceiver(rtpReceiver, track, mid)

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
					"isMigrated", isMigrated,
				)
			} else {
				p.pubLogger.Debugw(
					"track published",
					"trackID", mt.ID(),
					"track", logger.Proto(mt.ToProto()),
					"cost", pubTime.Milliseconds(),
					"isMigrated", isMigrated,
				)
			}

			prometheus.RecordPublishTime(
				p.params.Country,
				mt.Source(),
				mt.Kind(),
				pubTime,
				p.GetClientInfo().GetSdk(),
				p.Kind(),
			)
			p.handleTrackPublished(mt, isMigrated)
		}()
	}

	return mt, newTrack, isReceiverAdded, ridsFromSdp
}

func (p *ParticipantImpl) addMigratedTrack(cid string, ti *livekit.TrackInfo) *MediaTrack {
	p.pubLogger.Infow("add migrated track", "cid", cid, "trackID", ti.Sid, "track", logger.Proto(ti))
	rtpReceiver := p.TransportManager.GetPublisherRTPReceiver(ti.Mid)
	if rtpReceiver == nil {
		p.pubLogger.Errorw(
			"could not find receiver for migrated track", nil,
			"trackID", ti.Sid,
			"mid", ti.Mid,
		)
		return nil
	}

	mt := p.addMediaTrack(cid, ti)

	potentialCodecs := make([]webrtc.RTPCodecParameters, 0, len(ti.Codecs))
	parameters := rtpReceiver.GetParameters()
	for _, c := range ti.Codecs {
		for _, nc := range parameters.Codecs {
			if mime.IsMimeTypeStringEqual(nc.MimeType, c.MimeType) {
				potentialCodecs = append(potentialCodecs, nc)
				break
			}
		}
	}
	// check for mime_type for tracks that do not have simulcast_codecs set
	if ti.MimeType != "" {
		for _, nc := range parameters.Codecs {
			if mime.IsMimeTypeStringEqual(nc.MimeType, ti.MimeType) {
				alreadyAdded := false
				for _, pc := range potentialCodecs {
					if mime.IsMimeTypeStringEqual(pc.MimeType, ti.MimeType) {
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
				mt.SetLayerSsrc(mime.NormalizeMimeType(codec.MimeType), info.Rid, ssrc)
			}
		}
	}

	return mt
}

func (p *ParticipantImpl) addMediaTrack(signalCid string, ti *livekit.TrackInfo) *MediaTrack {
	mt := NewMediaTrack(MediaTrackParams{
		ParticipantID:         p.ID,
		ParticipantIdentity:   p.params.Identity,
		ParticipantVersion:    p.version.Load(),
		ParticipantCountry:    p.params.Country,
		BufferFactory:         p.params.Config.BufferFactory,
		ReceiverConfig:        p.params.Config.Receiver,
		AudioConfig:           p.params.AudioConfig,
		VideoConfig:           p.params.VideoConfig,
		Telemetry:             p.params.Telemetry,
		Logger:                LoggerWithTrack(p.pubLogger, livekit.TrackID(ti.Sid), false),
		Reporter:              p.params.Reporter.WithTrack(ti.Sid),
		SubscriberConfig:      p.params.Config.Subscriber,
		PLIThrottleConfig:     p.params.PLIThrottleConfig,
		SimTracks:             p.params.SimTracks,
		OnRTCP:                p.postRtcp,
		ForwardStats:          p.params.ForwardStats,
		OnTrackEverSubscribed: p.sendTrackHasBeenSubscribed,
		ShouldRegressCodec: func() bool {
			return p.helper().ShouldRegressCodec()
		},
		PreferVideoSizeFromMedia: p.params.PreferVideoSizeFromMedia,
	}, ti)

	mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)
	mt.OnSubscribedAudioCodecChange(p.onSubscribedAudioCodecChange)

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
	mt.AddOnClose(func(isExpectedToResume bool) {
		if p.supervisor != nil {
			p.supervisor.ClearPublishedTrack(trackID, mt)
		}

		if !isExpectedToResume {
			p.params.Telemetry.TrackUnpublished(
				context.Background(),
				p.ID(),
				p.Identity(),
				mt.ToProto(),
				true,
			)
		}

		p.pendingTracksLock.Lock()
		if pti := p.pendingTracks[signalCid]; pti != nil {
			p.sendTrackPublished(signalCid, pti.trackInfos[0])
			pti.queued = false
		}
		p.pendingTracksLock.Unlock()
		p.handlePendingRemoteTracks()

		p.dirty.Store(true)

		p.pubLogger.Debugw(
			"track unpublished",
			"trackID", ti.Sid,
			"expectedToResume", isExpectedToResume,
			"track", logger.Proto(ti),
		)
		if onTrackUnpublished := p.getOnTrackUnpublished(); onTrackUnpublished != nil {
			onTrackUnpublished(p, mt)
		}
	})

	return mt
}

func (p *ParticipantImpl) handleTrackPublished(track types.MediaTrack, isMigrated bool) {
	if onTrackPublished := p.getOnTrackPublished(); onTrackPublished != nil {
		onTrackPublished(p, track)
	}

	// send webhook after callbacks are complete, persistence and state handling happens
	// in `onTrackPublished` cb
	if !isMigrated {
		p.params.Telemetry.TrackPublished(
			context.Background(),
			p.ID(),
			p.Identity(),
			track.ToProto(),
		)
	}

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

func (p *ParticipantImpl) getPendingTrack(clientId string, kind livekit.TrackType, skipQueued bool) (string, *livekit.TrackInfo, buffer.VideoLayersRid, bool, time.Time) {
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
		return signalCid, nil, buffer.VideoLayersRid{}, false, time.Time{}
	}

	return signalCid, utils.CloneProto(pendingInfo.trackInfos[0]), pendingInfo.sdpRids, pendingInfo.migrated, pendingInfo.createdAt
}

func (p *ParticipantImpl) getPendingTrackPrimaryBySdpCid(sdpCid string) *pendingTrackInfo {
	for _, pti := range p.pendingTracks {
		ti := pti.trackInfos[0]
		if len(ti.Codecs) == 0 {
			continue
		}
		if ti.Codecs[0].Cid == sdpCid || ti.Codecs[0].SdpCid == sdpCid {
			return pti
		}
	}

	return nil
}

// setTrackID either generates a new TrackID for an AddTrackRequest
func (p *ParticipantImpl) setTrackID(cid string, info *livekit.TrackInfo) {
	var trackID string
	// if already pending, use the same SID
	// it is possible to have multiple AddTrackRequests for the same track
	if pti := p.pendingTracks[cid]; pti != nil {
		trackID = pti.trackInfos[0].Sid
	}

	// otherwise generate
	if trackID == "" {
		trackPrefix := utils.TrackPrefix
		switch info.Type {
		case livekit.TrackType_VIDEO:
			trackPrefix += "V"
		case livekit.TrackType_AUDIO:
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
		if publishedTrack.(types.LocalMediaTrack).HasSignalCid(clientId) {
			p.pubLogger.Debugw("found track by signal cid", "signalCid", clientId, "trackID", publishedTrack.ID())
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
		"ID":    p.ID(),
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
	if fs, err := p.helper().GetSubscriberForwarderState(p); err == nil && fs != nil {
		p.lock.Lock()
		p.forwarderState = fs
		p.lock.Unlock()

		for _, t := range p.SubscriptionManager.GetSubscribedTracks() {
			if dt := t.DownTrack(); dt != nil {
				dt.SeedState(sfu.DownTrackState{ForwarderState: p.getAndDeleteForwarderState(t.ID())})
			}
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

	p.sendSubscriptionResponse(trackID, signalErr)

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

func (p *ParticipantImpl) UpdateSubscribedAudioCodecs(nodeID livekit.NodeID, trackID livekit.TrackID, codecs []*livekit.SubscribedAudioCodec) error {
	track := p.GetPublishedTrack(trackID)
	if track == nil {
		p.pubLogger.Debugw("could not find track", "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.(types.LocalMediaTrack).NotifySubscriptionNode(nodeID, codecs)
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
	return p.ProtocolVersion().SupportsSyncStreamID() && !p.params.ClientInfo.isFirefox() && p.params.SyncStreams
}

func (p *ParticipantImpl) SupportsTransceiverReuse() bool {
	if p.params.UseOneShotSignallingMode {
		return p.ProtocolVersion().SupportsTransceiverReuse()
	}

	return p.ProtocolVersion().SupportsTransceiverReuse() && !p.SupportsSyncStreamID()
}

func (p *ParticipantImpl) SendDataMessage(kind livekit.DataPacket_Kind, data []byte, sender livekit.ParticipantID, seq uint32) error {
	if sender == "" || kind != livekit.DataPacket_RELIABLE || seq == 0 {
		if p.State() != livekit.ParticipantInfo_ACTIVE {
			return ErrDataChannelUnavailable
		}
		return p.TransportManager.SendDataMessage(kind, data)
	}

	p.reliableDataInfo.joiningMessageLock.Lock()
	if !p.reliableDataInfo.canWriteReliable {
		if _, ok := p.reliableDataInfo.joiningMessageFirstSeqs[sender]; !ok {
			p.reliableDataInfo.joiningMessageFirstSeqs[sender] = seq
		}
		p.reliableDataInfo.joiningMessageLock.Unlock()
		return nil
	}

	lastWrittenSeq, ok := p.reliableDataInfo.joiningMessageLastWrittenSeqs[sender]
	if ok {
		if seq <= lastWrittenSeq {
			// already sent by replayJoiningReliableMessages
			p.reliableDataInfo.joiningMessageLock.Unlock()
			return nil
		} else {
			delete(p.reliableDataInfo.joiningMessageLastWrittenSeqs, sender)
		}
	}

	p.reliableDataInfo.joiningMessageLock.Unlock()

	return p.TransportManager.SendDataMessage(kind, data)
}

func (p *ParticipantImpl) SendDataMessageUnlabeled(data []byte, useRaw bool, sender livekit.ParticipantIdentity) error {
	if p.State() != livekit.ParticipantInfo_ACTIVE {
		return ErrDataChannelUnavailable
	}

	return p.TransportManager.SendDataMessageUnlabeled(data, useRaw, sender)
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
			if mime.IsMimeTypeStringEqual(c.Mime, disableCodec.Mime) {
				return true
			}
		}
		return false
	}

	publishCodecsAudio := make([]*livekit.Codec, 0, len(publishEnabledCodecs))
	publishCodecsVideo := make([]*livekit.Codec, 0, len(publishEnabledCodecs))
	for _, c := range publishEnabledCodecs {
		if shouldDisable(c, disabledCodecs.GetCodecs()) || shouldDisable(c, disabledCodecs.GetPublish()) {
			continue
		}

		// sort by compatibility, since we will look for backups in these.
		if mime.IsMimeTypeStringVP8(c.Mime) {
			if len(p.enabledPublishCodecs) > 0 {
				p.enabledPublishCodecs = slices.Insert(p.enabledPublishCodecs, 0, c)
			} else {
				p.enabledPublishCodecs = append(p.enabledPublishCodecs, c)
			}
		} else if mime.IsMimeTypeStringH264(c.Mime) {
			p.enabledPublishCodecs = append(p.enabledPublishCodecs, c)
		} else {
			if mime.IsMimeTypeStringAudio(c.Mime) {
				publishCodecsAudio = append(publishCodecsAudio, c)
			} else {
				publishCodecsVideo = append(publishCodecsVideo, c)
			}
		}
	}
	// list all video first and then audio to work around a client side issue with Flutter SDK 2.4.2
	p.enabledPublishCodecs = append(p.enabledPublishCodecs, publishCodecsVideo...)
	p.enabledPublishCodecs = append(p.enabledPublishCodecs, publishCodecsAudio...)

	subscribeCodecs := make([]*livekit.Codec, 0, len(subscribeEnabledCodecs))
	for _, c := range subscribeEnabledCodecs {
		if shouldDisable(c, disabledCodecs.GetCodecs()) {
			continue
		}
		subscribeCodecs = append(subscribeCodecs, c)
	}
	p.enabledSubscribeCodecs = subscribeCodecs
	p.params.Logger.Debugw(
		"setup enabled codecs",
		"publish", logger.ProtoSlice(p.enabledPublishCodecs),
		"subscribe", logger.ProtoSlice(p.enabledSubscribeCodecs),
		"disabled", logger.Proto(disabledCodecs),
	)
}

func (p *ParticipantImpl) replayJoiningReliableMessages() {
	p.reliableDataInfo.joiningMessageLock.Lock()
	for _, msgCache := range p.helper().GetCachedReliableDataMessage(p.reliableDataInfo.joiningMessageFirstSeqs) {
		if len(msgCache.DestIdentities) != 0 && !slices.Contains(msgCache.DestIdentities, p.Identity()) {
			continue
		}
		if lastSeq, ok := p.reliableDataInfo.joiningMessageLastWrittenSeqs[msgCache.SenderID]; !ok || lastSeq < msgCache.Seq {
			p.reliableDataInfo.joiningMessageLastWrittenSeqs[msgCache.SenderID] = msgCache.Seq
		}

		p.TransportManager.SendDataMessage(livekit.DataPacket_RELIABLE, msgCache.Data)
	}

	p.reliableDataInfo.joiningMessageFirstSeqs = make(map[livekit.ParticipantID]uint32)
	p.reliableDataInfo.canWriteReliable = true
	p.reliableDataInfo.joiningMessageLock.Unlock()
}

func (p *ParticipantImpl) GetEnabledPublishCodecs() []*livekit.Codec {
	codecs := make([]*livekit.Codec, 0, len(p.enabledPublishCodecs))
	for _, c := range p.enabledPublishCodecs {
		if mime.IsMimeTypeStringRTX(c.Mime) {
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

				ti.AudioFeatures = sutils.DedupeSlice(update.Features)
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
	p.sendRequestResponse(&livekit.RequestResponse{
		Reason: livekit.RequestResponse_NOT_FOUND,
		Request: &livekit.RequestResponse_UpdateAudioTrack{
			UpdateAudioTrack: utils.CloneProto(update),
		},
	})
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
	p.sendRequestResponse(&livekit.RequestResponse{
		Reason: livekit.RequestResponse_NOT_FOUND,
		Request: &livekit.RequestResponse_UpdateVideoTrack{
			UpdateVideoTrack: utils.CloneProto(update),
		},
	})
	return errors.New("could not find track")
}

func (p *ParticipantImpl) HandleMetrics(senderParticipantID livekit.ParticipantID, metrics *livekit.MetricsBatch) error {
	if p.State() != livekit.ParticipantInfo_ACTIVE {
		return ErrDataChannelUnavailable
	}

	if !p.CanSubscribeMetrics() {
		return ErrNoSubscribeMetricsPermission
	}

	if senderParticipantID != p.ID() && !p.SubscriptionManager.IsSubscribedTo(senderParticipantID) {
		return nil
	}

	p.metricsReporter.Merge(metrics)
	return nil
}

func (p *ParticipantImpl) SupportsCodecChange() bool {
	return p.params.ClientInfo.SupportsCodecChange()
}

func (p *ParticipantImpl) SupportsMoving() error {
	if !p.ProtocolVersion().SupportsMoving() {
		return ErrMoveOldClientVersion
	}

	if kind := p.Kind(); kind == livekit.ParticipantInfo_EGRESS || kind == livekit.ParticipantInfo_AGENT || p.params.UseOneShotSignallingMode {
		return fmt.Errorf("%s participants cannot be moved, one-shot signaling mode: %t", kind.String(), p.params.UseOneShotSignallingMode)
	}

	return nil
}

func (p *ParticipantImpl) MoveToRoom(params types.MoveToRoomParams) {
	// fire onClose callback for original room
	p.lock.Lock()
	onClose := p.onClose
	p.onClose = make(map[string]func(types.LocalParticipant))
	p.lock.Unlock()
	for _, cb := range onClose {
		cb(p)
	}

	for _, track := range p.GetPublishedTracks() {
		for _, sub := range track.GetAllSubscribers() {
			track.RemoveSubscriber(sub, false)
		}

		// clear the subscriber node max quality/audio codecs as the remote quality notify
		// from source room would not reach the moving out participant.
		track.(types.LocalMediaTrack).ClearSubscriberNodes()

		trackInfo := track.ToProto()
		p.params.Telemetry.TrackUnpublished(
			context.Background(),
			p.ID(),
			p.Identity(),
			trackInfo,
			true,
		)
	}

	p.params.Logger.Infow("move participant to new room", "newRoomName", params.RoomName, "newID", params.ParticipantID)

	p.params.LoggerResolver.Reset()
	p.params.ReporterResolver.Reset()
	p.participantHelper.Store(params.Helper)
	p.SubscriptionManager.ClearAllSubscriptions()
	p.id.Store(params.ParticipantID)
	grants := p.grants.Load().Clone()
	grants.Video.Room = string(params.RoomName)
	p.grants.Store(grants)
}

func (p *ParticipantImpl) helper() types.LocalParticipantHelper {
	return p.participantHelper.Load().(types.LocalParticipantHelper)
}

func (p *ParticipantImpl) GetLastReliableSequence(migrateOut bool) uint32 {
	if migrateOut {
		p.reliableDataInfo.stopReliableByMigrateOut.Store(true)
	}
	return p.reliableDataInfo.lastPubReliableSeq.Load()
}

func (p *ParticipantImpl) HandleUpdateSubscriptions(
	trackIDs []livekit.TrackID,
	participantTracks []*livekit.ParticipantTracks,
	subscribe bool,
) {
	if onUpdateSubscriptions := p.getOnUpdateSubscriptions(); onUpdateSubscriptions != nil {
		onUpdateSubscriptions(p, trackIDs, participantTracks, subscribe)
	}
}

func (p *ParticipantImpl) HandleUpdateSubscriptionPermission(subscriptionPermission *livekit.SubscriptionPermission) error {
	if onUpdateSubscriptionPermission := p.getOnUpdateSubscriptionPermission(); onUpdateSubscriptionPermission != nil {
		return onUpdateSubscriptionPermission(p, subscriptionPermission)
	}

	return errors.New("no handler")
}

func (p *ParticipantImpl) HandleSyncState(syncState *livekit.SyncState) error {
	if onSyncState := p.getOnSyncState(); onSyncState != nil {
		return onSyncState(p, syncState)
	}

	return errors.New("no handler")
}

func (p *ParticipantImpl) HandleSimulateScenario(simulateScenario *livekit.SimulateScenario) error {
	if onSimulateScenario := p.getOnSimulateScenario(); onSimulateScenario != nil {
		return onSimulateScenario(p, simulateScenario)
	}

	return errors.New("no handler")
}

func (p *ParticipantImpl) HandleLeaveRequest(reason types.ParticipantCloseReason) {
	if onLeave := p.getOnLeave(); onLeave != nil {
		onLeave(p, reason)
	}
}

func (p *ParticipantImpl) HandleSignalMessage(msg proto.Message) error {
	return p.signalHandler.HandleMessage(msg)
}

func (p *ParticipantImpl) IsUsingSinglePeerConnection() bool {
	return p.params.UseSinglePeerConnection
}

func (p *ParticipantImpl) AddTrackLocal(
	trackLocal webrtc.TrackLocal,
	params types.AddTrackParams,
) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	if p.params.UseSinglePeerConnection {
		return p.TransportManager.AddTrackLocal(
			trackLocal,
			params,
			p.enabledSubscribeCodecs,
			p.params.Config.Subscriber.RTCPFeedback,
		)
	} else {
		return p.TransportManager.AddTrackLocal(trackLocal, params, nil, RTCPFeedbackConfig{})
	}
}

func (p *ParticipantImpl) AddTransceiverFromTrackLocal(
	trackLocal webrtc.TrackLocal,
	params types.AddTrackParams,
) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	if p.params.UseSinglePeerConnection {
		return p.TransportManager.AddTransceiverFromTrackLocal(
			trackLocal,
			params,
			p.enabledSubscribeCodecs,
			p.params.Config.Subscriber.RTCPFeedback,
		)
	} else {
		return p.TransportManager.AddTransceiverFromTrackLocal(
			trackLocal,
			params,
			nil,
			RTCPFeedbackConfig{},
		)
	}
}

func (p *ParticipantImpl) handleIncomingRpcAck(requestId string) bool {
	p.rpcLock.Lock()
	defer p.rpcLock.Unlock()

	handler, ok := p.rpcPendingAcks[requestId]
	if !ok {
		return false
	}

	handler.Resolve()
	delete(p.rpcPendingAcks, requestId)
	return true
}

func (p *ParticipantImpl) handleIncomingRpcResponse(requestId string, payload string, err *utils.DataChannelRpcError) bool {
	p.rpcLock.Lock()
	defer p.rpcLock.Unlock()

	handler, ok := p.rpcPendingResponses[requestId]
	if !ok {
		return false
	}

	handler.Resolve(payload, err)
	delete(p.rpcPendingResponses, requestId)
	return true
}

func (p *ParticipantImpl) PerformRpc(req *livekit.PerformRpcRequest, resultCh chan string, errorCh chan error) {
	responseTimeout := req.GetResponseTimeoutMs()
	if responseTimeout <= 0 {
		responseTimeout = uint32(utils.DataChannelRpcDefaultResponseTimeout.Milliseconds())
	}

	go func() {
		if len([]byte(req.GetPayload())) > utils.DataChannelRpcMaxPayloadBytes {
			errorCh <- utils.DataChannelRpcErrorFromBuiltInCodes(utils.DataChannelRpcRequestPayloadTooLarge, "").PsrpcError()
			return
		}

		id := uuid.NewString()

		responseTimer := time.AfterFunc(time.Duration(responseTimeout)*time.Millisecond, func() {
			p.rpcLock.Lock()
			delete(p.rpcPendingResponses, id)
			p.rpcLock.Unlock()

			select {
			case errorCh <- utils.DataChannelRpcErrorFromBuiltInCodes(utils.DataChannelRpcResponseTimeout, "").PsrpcError():
			default:
			}
		})
		ackTimer := time.AfterFunc(utils.DataChannelRpcMaxRoundTripLatency, func() {
			p.rpcLock.Lock()
			delete(p.rpcPendingAcks, id)
			delete(p.rpcPendingResponses, id)
			p.rpcLock.Unlock()
			responseTimer.Stop()

			select {
			case errorCh <- utils.DataChannelRpcErrorFromBuiltInCodes(utils.DataChannelRpcConnectionTimeout, "").PsrpcError():
			default:
			}
		})

		rpcRequest := &livekit.DataPacket{
			Kind:                livekit.DataPacket_RELIABLE,
			ParticipantIdentity: id,
			Value: &livekit.DataPacket_RpcRequest{
				RpcRequest: &livekit.RpcRequest{
					Id:                id,
					Method:            req.GetMethod(),
					Payload:           req.GetPayload(),
					ResponseTimeoutMs: responseTimeout - p.lastRTT,
					Version:           1,
				},
			},
		}
		data, err := proto.Marshal(rpcRequest)
		if err != nil {
			ackTimer.Stop()
			responseTimer.Stop()
			errorCh <- psrpc.NewError(psrpc.Internal, err)
			return
		}

		// using RPC ID as the unique ID for server to identify the response
		err = p.SendDataMessage(livekit.DataPacket_RELIABLE, data, livekit.ParticipantID(id), 0)
		if err != nil {
			ackTimer.Stop()
			responseTimer.Stop()
			errorCh <- psrpc.NewError(psrpc.Internal, err)
			return
		}

		p.rpcLock.Lock()
		p.rpcPendingAcks[id] = &utils.DataChannelRpcPendingAckHandler{
			Resolve: func() {
				ackTimer.Stop()
			},
			ParticipantIdentity: req.GetDestinationIdentity(),
		}
		p.rpcPendingResponses[id] = &utils.DataChannelRpcPendingResponseHandler{
			Resolve: func(payload string, error *utils.DataChannelRpcError) {
				responseTimer.Stop()
				if _, ok := p.rpcPendingAcks[id]; ok {
					p.rpcPendingAcks[id].Resolve()
					ackTimer.Stop()
				}

				if error != nil {
					errorCh <- error.PsrpcError()
				} else {
					resultCh <- payload
				}
			},
			ParticipantIdentity: req.GetDestinationIdentity(),
		}
		p.rpcLock.Unlock()
	}()
}
