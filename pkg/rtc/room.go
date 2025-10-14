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
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	sutils "github.com/livekit/livekit-server/pkg/utils"
)

const (
	AudioLevelQuantization    = 8 // ideally power of 2 to minimize float decimal
	invAudioLevelQuantization = 1.0 / AudioLevelQuantization
	subscriberUpdateInterval  = 3 * time.Second

	dataForwardLoadBalanceThreshold = 4

	simulateDisconnectSignalTimeout = 5 * time.Second

	dataMessageCacheTTL  = 2 * time.Second
	dataMessageCacheSize = 100_000
)

var (
	// var to allow unit test override
	roomUpdateInterval = 5 * time.Second // frequency to update room participant counts

	ErrJobShutdownTimeout = psrpc.NewErrorf(psrpc.DeadlineExceeded, "timed out waiting for agent job to shutdown")
)

// Duplicate the service.AgentStore interface to avoid a rtc -> service -> rtc import cycle
type AgentStore interface {
	StoreAgentDispatch(ctx context.Context, dispatch *livekit.AgentDispatch) error
	DeleteAgentDispatch(ctx context.Context, dispatch *livekit.AgentDispatch) error
	ListAgentDispatches(ctx context.Context, roomName livekit.RoomName) ([]*livekit.AgentDispatch, error)

	StoreAgentJob(ctx context.Context, job *livekit.Job) error
	DeleteAgentJob(ctx context.Context, job *livekit.Job) error
}

type broadcastOptions struct {
	skipSource bool
	immediate  bool
}

type disconnectSignalOnResumeNoMessages struct {
	expiry      time.Time
	closedCount int
}

type Room struct {
	// atomics always need to be 64bit/8byte aligned
	// on 32bit arch only the beginning of the struct
	// starts at such a boundary.
	// time the first participant joined the room
	joinedAt atomic.Int64
	// time that the last participant left the room
	leftAt atomic.Int64
	holds  atomic.Int32

	lock sync.RWMutex

	protoRoom  *livekit.Room
	internal   *livekit.RoomInternal
	protoProxy *utils.ProtoProxy[*livekit.Room]
	logger     logger.Logger

	config          WebRTCConfig
	roomConfig      config.RoomConfig
	audioConfig     *sfu.AudioConfig
	serverInfo      *livekit.ServerInfo
	telemetry       telemetry.TelemetryService
	egressLauncher  EgressLauncher
	trackManager    *RoomTrackManager
	agentDispatches map[string]*agentDispatch

	// agents
	agentClient agent.Client
	agentStore  AgentStore

	// map of identity -> Participant
	participants              map[livekit.ParticipantIdentity]types.LocalParticipant
	participantOpts           map[livekit.ParticipantIdentity]*ParticipantOptions
	participantRequestSources map[livekit.ParticipantIdentity]routing.MessageSource
	hasPublished              map[livekit.ParticipantIdentity]bool
	agentParticpants          map[livekit.ParticipantIdentity]*agentJob
	bufferFactory             *buffer.FactoryOfBufferFactory

	// batch update participant info for non-publishers
	batchedUpdates   map[livekit.ParticipantIdentity]*ParticipantUpdate
	batchedUpdatesMu sync.Mutex

	closed chan struct{}

	trailer []byte

	onParticipantChanged func(p types.LocalParticipant)
	onRoomUpdated        func()
	onClose              func()

	simulationLock                                 sync.Mutex
	disconnectSignalOnResumeParticipants           map[livekit.ParticipantIdentity]time.Time
	disconnectSignalOnResumeNoMessagesParticipants map[livekit.ParticipantIdentity]*disconnectSignalOnResumeNoMessages

	userPacketDeduper *UserPacketDeduper

	dataMessageCache *utils.TimeSizeCache[types.DataMessageCache]
}

type ParticipantOptions struct {
	AutoSubscribe bool
}

type agentDispatch struct {
	*livekit.AgentDispatch
	lock    sync.Mutex
	pending map[chan struct{}]struct{}
}

type agentJob struct {
	*livekit.Job
	lock sync.Mutex
	done chan struct{}
}

// This provides utilities attached the agent dispatch to ensure that all pending jobs are created
// before terminating jobs attached to an agent dispatch. This avoids a race that could cause some pending jobs
// to not be terminated when a dispatch is deleted.
func newAgentDispatch(ad *livekit.AgentDispatch) *agentDispatch {
	return &agentDispatch{
		AgentDispatch: ad,
		pending:       make(map[chan struct{}]struct{}),
	}
}

func (ad *agentDispatch) jobsLaunching() (jobsLaunched func()) {
	ad.lock.Lock()
	c := make(chan struct{})
	ad.pending[c] = struct{}{}
	ad.lock.Unlock()

	return func() {
		close(c)
		ad.lock.Lock()
		delete(ad.pending, c)
		ad.lock.Unlock()
	}
}

func (ad *agentDispatch) waitForPendingJobs() {
	ad.lock.Lock()
	cs := maps.Keys(ad.pending)
	ad.lock.Unlock()

	for _, c := range cs {
		<-c
	}
}

// This provides utilities to ensure that an agent left the room when killing a job
func newAgentJob(j *livekit.Job) *agentJob {
	return &agentJob{
		Job:  j,
		done: make(chan struct{}),
	}
}

func (j *agentJob) participantLeft() {
	j.lock.Lock()
	if j.done != nil {
		close(j.done)
		j.done = nil
	}
	j.lock.Unlock()
}

func (j *agentJob) waitForParticipantLeaving() error {
	var done chan struct{}

	j.lock.Lock()
	done = j.done
	j.lock.Unlock()

	if done != nil {
		select {
		case <-done:
			return nil
		case <-time.After(3 * time.Second):
			return ErrJobShutdownTimeout
		}
	}

	return nil
}

func NewRoom(
	room *livekit.Room,
	internal *livekit.RoomInternal,
	config WebRTCConfig,
	roomConfig config.RoomConfig,
	audioConfig *sfu.AudioConfig,
	serverInfo *livekit.ServerInfo,
	telemetry telemetry.TelemetryService,
	agentClient agent.Client,
	agentStore AgentStore,
	egressLauncher EgressLauncher,
) *Room {
	r := &Room{
		protoRoom: utils.CloneProto(room),
		internal:  internal,
		logger: LoggerWithRoom(
			logger.GetLogger().WithComponent(sutils.ComponentRoom),
			livekit.RoomName(room.Name),
			livekit.RoomID(room.Sid),
		),
		config:                               config,
		roomConfig:                           roomConfig,
		audioConfig:                          audioConfig,
		telemetry:                            telemetry,
		egressLauncher:                       egressLauncher,
		agentClient:                          agentClient,
		agentStore:                           agentStore,
		agentDispatches:                      make(map[string]*agentDispatch),
		trackManager:                         NewRoomTrackManager(),
		serverInfo:                           serverInfo,
		participants:                         make(map[livekit.ParticipantIdentity]types.LocalParticipant),
		participantOpts:                      make(map[livekit.ParticipantIdentity]*ParticipantOptions),
		participantRequestSources:            make(map[livekit.ParticipantIdentity]routing.MessageSource),
		hasPublished:                         make(map[livekit.ParticipantIdentity]bool),
		agentParticpants:                     make(map[livekit.ParticipantIdentity]*agentJob),
		bufferFactory:                        buffer.NewFactoryOfBufferFactory(config.Receiver.PacketBufferSizeVideo, config.Receiver.PacketBufferSizeAudio),
		batchedUpdates:                       make(map[livekit.ParticipantIdentity]*ParticipantUpdate),
		closed:                               make(chan struct{}),
		trailer:                              []byte(utils.RandomSecret()),
		disconnectSignalOnResumeParticipants: make(map[livekit.ParticipantIdentity]time.Time),
		disconnectSignalOnResumeNoMessagesParticipants: make(map[livekit.ParticipantIdentity]*disconnectSignalOnResumeNoMessages),
		userPacketDeduper: NewUserPacketDeduper(),
		dataMessageCache: utils.NewTimeSizeCache[types.DataMessageCache](utils.TimeSizeCacheParams{
			TTL:     dataMessageCacheTTL,
			MaxSize: dataMessageCacheSize,
		}),
	}

	if r.protoRoom.EmptyTimeout == 0 {
		r.protoRoom.EmptyTimeout = roomConfig.EmptyTimeout
	}
	if r.protoRoom.DepartureTimeout == 0 {
		r.protoRoom.DepartureTimeout = roomConfig.DepartureTimeout
	}
	if r.protoRoom.CreationTime == 0 {
		now := time.Now()
		r.protoRoom.CreationTime = now.Unix()
		r.protoRoom.CreationTimeMs = now.UnixMilli()
	}
	r.protoProxy = utils.NewProtoProxy(roomUpdateInterval, r.updateProto)

	r.createAgentDispatchesFromRoomAgent()

	r.launchRoomAgents(maps.Values(r.agentDispatches))

	go r.audioUpdateWorker()
	go r.connectionQualityWorker()
	go r.changeUpdateWorker()
	go r.simulationCleanupWorker()

	return r
}

func (r *Room) Logger() logger.Logger {
	return r.logger
}

func (r *Room) ToProto() *livekit.Room {
	return r.protoProxy.Get()
}

func (r *Room) Name() livekit.RoomName {
	return livekit.RoomName(r.protoRoom.Name)
}

func (r *Room) ID() livekit.RoomID {
	return livekit.RoomID(r.protoRoom.Sid)
}

func (r *Room) Trailer() []byte {
	r.lock.RLock()
	defer r.lock.RUnlock()

	trailer := make([]byte, len(r.trailer))
	copy(trailer, r.trailer)
	return trailer
}

func (r *Room) GetParticipant(identity livekit.ParticipantIdentity) types.LocalParticipant {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.participants[identity]
}

func (r *Room) GetParticipantByID(participantID livekit.ParticipantID) types.LocalParticipant {
	r.lock.RLock()
	defer r.lock.RUnlock()

	for _, p := range r.participants {
		if p.ID() == participantID {
			return p
		}
	}

	return nil
}

func (r *Room) GetParticipants() []types.LocalParticipant {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return maps.Values(r.participants)
}

func (r *Room) GetLocalParticipants() []types.LocalParticipant {
	return r.GetParticipants()
}

func (r *Room) GetParticipantCount() int {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return len(r.participants)
}

func (r *Room) GetActiveSpeakers() []*livekit.SpeakerInfo {
	participants := r.GetParticipants()
	speakers := make([]*livekit.SpeakerInfo, 0, len(participants))
	for _, p := range participants {
		level, active := p.GetAudioLevel()
		if !active {
			continue
		}
		speakers = append(speakers, &livekit.SpeakerInfo{
			Sid:    string(p.ID()),
			Level:  float32(level),
			Active: active,
		})
	}

	sort.Slice(speakers, func(i, j int) bool {
		return speakers[i].Level > speakers[j].Level
	})

	// quantize to smooth out small changes
	for _, speaker := range speakers {
		speaker.Level = float32(math.Ceil(float64(speaker.Level*AudioLevelQuantization)) * invAudioLevelQuantization)
	}

	return speakers
}

func (r *Room) GetBufferFactory() *buffer.Factory {
	return r.bufferFactory.CreateBufferFactory()
}

func (r *Room) FirstJoinedAt() int64 {
	return r.joinedAt.Load()
}

func (r *Room) LastLeftAt() int64 {
	return r.leftAt.Load()
}

func (r *Room) Internal() *livekit.RoomInternal {
	return r.internal
}

func (r *Room) Hold() bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.IsClosed() {
		return false
	}

	r.holds.Inc()
	return true
}

func (r *Room) Release() {
	r.holds.Dec()
}

func (r *Room) Join(
	participant types.LocalParticipant,
	requestSource routing.MessageSource,
	opts *ParticipantOptions,
	iceServers []*livekit.ICEServer,
) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.IsClosed() {
		return ErrRoomClosed
	}

	if r.participants[participant.Identity()] != nil {
		return ErrAlreadyJoined
	}
	if r.protoRoom.MaxParticipants > 0 && !participant.IsDependent() {
		numParticipants := uint32(0)
		for _, p := range r.participants {
			if !p.IsDependent() {
				numParticipants++
			}
		}
		if numParticipants >= r.protoRoom.MaxParticipants {
			return ErrMaxParticipantsExceeded
		}
	}

	if r.FirstJoinedAt() == 0 && !participant.IsDependent() {
		r.joinedAt.Store(time.Now().Unix())
	}

	var onStateChangeMu sync.Mutex
	participant.OnStateChange(func(p types.LocalParticipant) {
		if r.onParticipantChanged != nil {
			r.onParticipantChanged(p)
		}
		r.broadcastParticipantState(p, broadcastOptions{skipSource: true})

		onStateChangeMu.Lock()
		defer onStateChangeMu.Unlock()
		if state := p.State(); state == livekit.ParticipantInfo_ACTIVE {
			// subscribe participant to existing published tracks
			r.subscribeToExistingTracks(p, false)

			connectTime := time.Since(p.ConnectedAt())
			meta := &livekit.AnalyticsClientMeta{
				ClientConnectTime: uint32(connectTime.Milliseconds()),
			}
			infos := p.GetICEConnectionInfo()
			var connectionType roomobs.ConnectionType
			for _, info := range infos {
				if info.Type != types.ICEConnectionTypeUnknown {
					meta.ConnectionType = info.Type.String()
					connectionType = info.Type.ReporterType()
					break
				}
			}
			r.telemetry.ParticipantActive(context.Background(),
				r.ToProto(),
				p.ToProto(),
				meta,
				false,
				participant.TelemetryGuard(),
			)

			participant.GetReporter().Tx(func(tx roomobs.ParticipantSessionTx) {
				tx.ReportClientConnectTime(uint16(connectTime.Milliseconds()))
				tx.ReportConnectResult(roomobs.ConnectionResultSuccess)
				tx.ReportConnectionType(connectionType)
			})

			fields := append(
				connectionDetailsFields(infos),
				"clientInfo", logger.Proto(sutils.ClientInfoWithoutAddress(p.GetClientInfo())),
			)
			p.GetLogger().Infow("participant active", fields...)
		} else if state == livekit.ParticipantInfo_DISCONNECTED {
			// remove participant from room
			go r.RemoveParticipant(p.Identity(), p.ID(), p.CloseReason())
		}
	})
	participant.OnSubscriberReady(func(p types.LocalParticipant) {
		r.subscribeToExistingTracks(p, false)
	})
	// it's important to set this before connection, we don't want to miss out on any published tracks
	participant.OnTrackPublished(r.onTrackPublished)
	participant.OnTrackUpdated(r.onTrackUpdated)
	participant.OnTrackUnpublished(r.onTrackUnpublished)
	participant.OnParticipantUpdate(r.onParticipantUpdate)
	participant.OnDataPacket(r.onDataPacket)
	participant.OnDataMessage(r.onDataMessage)
	participant.OnMetrics(r.onMetrics)
	participant.OnSubscribeStatusChanged(func(publisherID livekit.ParticipantID, subscribed bool) {
		if subscribed {
			pub := r.GetParticipantByID(publisherID)
			if pub != nil && pub.State() == livekit.ParticipantInfo_ACTIVE {
				// when a participant subscribes to another participant,
				// send speaker update if the subscribed to participant is active.
				level, active := pub.GetAudioLevel()
				if active {
					_ = participant.SendSpeakerUpdate([]*livekit.SpeakerInfo{
						{
							Sid:    string(pub.ID()),
							Level:  float32(level),
							Active: active,
						},
					}, false)
				}

				if cq := pub.GetConnectionQuality(); cq != nil {
					update := &livekit.ConnectionQualityUpdate{}
					update.Updates = append(update.Updates, cq)
					_ = participant.SendConnectionQualityUpdate(update)
				}
			}
		} else {
			// no longer subscribed to the publisher, clear speaker status
			_ = participant.SendSpeakerUpdate([]*livekit.SpeakerInfo{
				{
					Sid:    string(publisherID),
					Level:  0,
					Active: false,
				},
			}, true)
		}
	})
	participant.OnUpdateSubscriptions(r.onUpdateSubscriptions)
	participant.OnUpdateSubscriptionPermission(r.onUpdateSubscriptionPermission)
	participant.OnSyncState(r.onSyncState)
	participant.OnSimulateScenario(r.onSimulateScenario)
	participant.OnLeave(r.onLeave)

	r.launchTargetAgents(maps.Values(r.agentDispatches), participant, livekit.JobType_JT_PARTICIPANT)

	r.logger.Debugw(
		"new participant joined",
		"pID", participant.ID(),
		"participant", participant.Identity(),
		"clientInfo", logger.Proto(participant.GetClientInfo()),
		"options", opts,
		"numParticipants", len(r.participants),
	)

	if participant.IsRecorder() && !r.protoRoom.ActiveRecording {
		r.protoRoom.ActiveRecording = true
		r.protoProxy.MarkDirty(true)
	} else {
		r.protoProxy.MarkDirty(false)
	}

	r.participants[participant.Identity()] = participant
	r.participantOpts[participant.Identity()] = opts
	r.participantRequestSources[participant.Identity()] = requestSource

	if r.onParticipantChanged != nil {
		r.onParticipantChanged(participant)
	}

	time.AfterFunc(time.Minute, func() {
		if !participant.Verify() {
			r.RemoveParticipant(participant.Identity(), participant.ID(), types.ParticipantCloseReasonJoinTimeout)
		}
	})

	joinResponse := r.createJoinResponseLocked(participant, iceServers)
	if err := participant.SendJoinResponse(joinResponse); err != nil {
		prometheus.RecordServiceOperationError("participant_join", "send_response")
		return err
	}

	participant.SetMigrateState(types.MigrateStateComplete)

	if participant.SubscriberAsPrimary() {
		// initiates sub connection as primary
		if participant.ProtocolVersion().SupportFastStart() {
			go func() {
				r.subscribeToExistingTracks(participant, false)
				participant.Negotiate(true)
			}()
		} else {
			participant.Negotiate(true)
		}
	} else {
		if participant.IsUsingSinglePeerConnection() {
			go r.subscribeToExistingTracks(participant, false)
		}
	}

	prometheus.RecordServiceOperationSuccess("participant_join")

	return nil
}

func (r *Room) ReplaceParticipantRequestSource(identity livekit.ParticipantIdentity, reqSource routing.MessageSource) {
	r.lock.Lock()
	if rs, ok := r.participantRequestSources[identity]; ok {
		rs.Close()
	}
	r.participantRequestSources[identity] = reqSource
	r.lock.Unlock()
}

func (r *Room) GetParticipantRequestSource(identity livekit.ParticipantIdentity) routing.MessageSource {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.participantRequestSources[identity]
}

func (r *Room) ResumeParticipant(
	p types.LocalParticipant,
	requestSource routing.MessageSource,
	responseSink routing.MessageSink,
	iceConfig *livekit.ICEConfig,
	iceServers []*livekit.ICEServer,
	reason livekit.ReconnectReason,
) error {
	r.ReplaceParticipantRequestSource(p.Identity(), requestSource)
	// close previous sink, and link to new one
	p.CloseSignalConnection(types.SignallingCloseReasonResume)
	p.SetResponseSink(responseSink)

	p.SetSignalSourceValid(true)

	// check for simulated signal disconnect on resume before sending any signal response messages
	r.simulationLock.Lock()
	if state, ok := r.disconnectSignalOnResumeNoMessagesParticipants[p.Identity()]; ok {
		// WARNING: this uses knowledge that service layer tries internally
		simulated := false
		if time.Now().Before(state.expiry) {
			state.closedCount++
			p.CloseSignalConnection(types.SignallingCloseReasonDisconnectOnResumeNoMessages)
			simulated = true
		}
		if state.closedCount == 3 {
			delete(r.disconnectSignalOnResumeNoMessagesParticipants, p.Identity())
		}
		if simulated {
			r.simulationLock.Unlock()
			return nil
		}
	}
	r.simulationLock.Unlock()

	if err := p.HandleReconnectAndSendResponse(reason, &livekit.ReconnectResponse{
		IceServers:          iceServers,
		ClientConfiguration: p.GetClientConfiguration(),
		ServerInfo:          r.serverInfo,
		LastMessageSeq:      p.GetLastReliableSequence(false),
	}); err != nil {
		return err
	}

	// include the local participant's info as well, since metadata could have been changed
	updates := GetOtherParticipantInfo(nil, false, toParticipants(r.GetParticipants()), false)
	if err := p.SendParticipantUpdate(updates); err != nil {
		return err
	}

	_ = p.SendRoomUpdate(r.ToProto())
	p.ICERestart(iceConfig)

	// check for simulated signal disconnect on resume
	r.simulationLock.Lock()
	if timeout, ok := r.disconnectSignalOnResumeParticipants[p.Identity()]; ok {
		if time.Now().Before(timeout) {
			p.CloseSignalConnection(types.SignallingCloseReasonDisconnectOnResume)
		}
		delete(r.disconnectSignalOnResumeParticipants, p.Identity())
	}
	r.simulationLock.Unlock()

	return nil
}

func (r *Room) HandleSyncState(participant types.LocalParticipant, state *livekit.SyncState) error {
	if state != nil {
		return r.onSyncState(participant, state)
	}

	return nil
}

func (r *Room) onSyncState(participant types.LocalParticipant, state *livekit.SyncState) error {
	pLogger := participant.GetLogger()
	pLogger.Infow("setting sync state", "state", logger.Proto(state))

	shouldReconnect := false
	pubTracks := state.GetPublishTracks()
	existingPubTracks := participant.GetPublishedTracks()
	for _, pubTrack := range pubTracks {
		// client may not have sent TrackInfo for each published track
		ti := pubTrack.Track
		if ti == nil {
			pLogger.Warnw("TrackInfo not sent during resume", nil)
			shouldReconnect = true
			break
		}

		found := false
		for _, existingPubTrack := range existingPubTracks {
			if existingPubTrack.ID() == livekit.TrackID(ti.Sid) {
				found = true
				break
			}
		}
		if !found {
			// is there a pending track?
			found = participant.GetPendingTrack(livekit.TrackID(ti.Sid)) != nil
		}
		if !found {
			pLogger.Warnw("unknown track during resume", nil, "trackID", ti.Sid)
			shouldReconnect = true
			break
		}
	}
	if shouldReconnect {
		pLogger.Warnw("unable to resume due to missing published tracks, starting full reconnect", nil)
		participant.IssueFullReconnect(types.ParticipantCloseReasonPublicationError)
		return nil
	}

	// synthesize a track setting for each disabled track,
	// can be set before addding subscriptions,
	// in fact it is done before so that setting can be updated immediately upon subscription.
	for _, trackSid := range state.TrackSidsDisabled {
		participant.UpdateSubscribedTrackSettings(livekit.TrackID(trackSid), &livekit.UpdateTrackSettings{Disabled: true})
	}

	participant.HandleUpdateSubscriptions(
		livekit.StringsAsIDs[livekit.TrackID](state.Subscription.TrackSids),
		state.Subscription.ParticipantTracks,
		state.Subscription.Subscribe,
	)
	return nil
}

func (r *Room) onUpdateSubscriptionPermission(participant types.LocalParticipant, subscriptionPermission *livekit.SubscriptionPermission) error {
	if err := participant.UpdateSubscriptionPermission(subscriptionPermission, utils.TimedVersion(0), r.GetParticipantByID); err != nil {
		return err
	}
	for _, track := range participant.GetPublishedTracks() {
		r.trackManager.NotifyTrackChanged(track.ID())
	}
	return nil
}

func (r *Room) ResolveMediaTrackForSubscriber(sub types.LocalParticipant, trackID livekit.TrackID) types.MediaResolverResult {
	res := types.MediaResolverResult{}

	info := r.trackManager.GetTrackInfo(trackID)
	res.TrackChangedNotifier = r.trackManager.GetOrCreateTrackChangeNotifier(trackID)

	if info == nil {
		return res
	}

	res.Track = info.Track
	res.TrackRemovedNotifier = r.trackManager.GetOrCreateTrackRemoveNotifier(trackID)
	res.PublisherIdentity = info.PublisherIdentity
	res.PublisherID = info.PublisherID

	pub := r.GetParticipantByID(info.PublisherID)
	// when publisher is not found, we will assume it doesn't have permission to access
	if pub != nil {
		res.HasPermission = IsParticipantExemptFromTrackPermissionsRestrictions(sub) || pub.HasPermission(trackID, sub.Identity())
	}

	return res
}

func (r *Room) IsClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

// CloseIfEmpty closes the room if all participants had left, or it's still empty past timeout
func (r *Room) CloseIfEmpty() {
	r.lock.Lock()

	if r.IsClosed() || r.holds.Load() > 0 {
		r.lock.Unlock()
		return
	}

	for _, p := range r.participants {
		if !p.IsDependent() {
			r.lock.Unlock()
			return
		}
	}

	var timeout uint32
	var elapsed int64
	var reason string
	if r.FirstJoinedAt() > 0 && r.LastLeftAt() > 0 {
		elapsed = time.Now().Unix() - r.LastLeftAt()
		// need to give time in case participant is reconnecting
		timeout = r.protoRoom.DepartureTimeout
		reason = "departure timeout"
	} else {
		elapsed = time.Now().Unix() - r.protoRoom.CreationTime
		timeout = r.protoRoom.EmptyTimeout
		reason = "empty timeout"
	}
	r.lock.Unlock()

	if elapsed >= int64(timeout) {
		r.Close(types.ParticipantCloseReasonRoomClosed)
		r.logger.Infow("closing idle room", "reason", reason)
	}
}

func (r *Room) Close(reason types.ParticipantCloseReason) {
	r.lock.Lock()
	select {
	case <-r.closed:
		r.lock.Unlock()
		return
	default:
		// fall through
	}
	close(r.closed)
	r.lock.Unlock()

	r.logger.Infow("closing room")
	for _, p := range r.GetParticipants() {
		_ = p.Close(true, reason, false)
	}

	r.protoProxy.Stop()

	if r.onClose != nil {
		r.onClose()
	}
}

func (r *Room) OnClose(f func()) {
	r.onClose = f
}

func (r *Room) OnParticipantChanged(f func(participant types.LocalParticipant)) {
	r.onParticipantChanged = f
}

func (r *Room) SendDataPacket(dp *livekit.DataPacket, kind livekit.DataPacket_Kind) {
	r.onDataPacket(nil, kind, dp)
}

func (r *Room) SetMetadata(metadata string) <-chan struct{} {
	r.lock.Lock()
	r.protoRoom.Metadata = metadata
	r.lock.Unlock()
	return r.protoProxy.MarkDirty(true)
}

func (r *Room) sendRoomUpdate() {
	roomInfo := r.ToProto()
	// Send update to participants
	for _, p := range r.GetParticipants() {
		if !p.IsReady() {
			continue
		}

		err := p.SendRoomUpdate(roomInfo)
		if err != nil {
			r.logger.Warnw("failed to send room update", err, "participant", p.Identity())
		}
	}
}

func (r *Room) GetAgentDispatches(dispatchID string) ([]*livekit.AgentDispatch, error) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	var ret []*livekit.AgentDispatch

	for _, ad := range r.agentDispatches {
		if dispatchID == "" || ad.Id == dispatchID {
			ret = append(ret, utils.CloneProto(ad.AgentDispatch))
		}
	}

	return ret, nil
}

func (r *Room) AddAgentDispatch(dispatch *livekit.AgentDispatch) (*livekit.AgentDispatch, error) {
	ad, err := r.createAgentDispatch(dispatch)
	if err != nil {
		return nil, err
	}

	r.launchRoomAgents([]*agentDispatch{ad})

	r.lock.RLock()
	// launchPublisherAgents starts a goroutine to send requests, so is safe to call locked
	for _, p := range r.participants {
		if p.IsPublisher() {
			r.launchTargetAgents([]*agentDispatch{ad}, p, livekit.JobType_JT_PUBLISHER)
		}
		r.launchTargetAgents([]*agentDispatch{ad}, p, livekit.JobType_JT_PARTICIPANT)
	}
	r.lock.RUnlock()

	return ad.AgentDispatch, nil
}

func (r *Room) DeleteAgentDispatch(dispatchID string) (*livekit.AgentDispatch, error) {
	r.lock.Lock()
	ad := r.agentDispatches[dispatchID]
	if ad == nil {
		r.lock.Unlock()
		return nil, psrpc.NewErrorf(psrpc.NotFound, "dispatch ID not found")
	}

	delete(r.agentDispatches, dispatchID)
	r.lock.Unlock()

	// Should Delete be synchronous instead?
	go func() {
		ad.waitForPendingJobs()

		var jobs []*livekit.Job
		r.lock.RLock()
		if ad.State != nil {
			jobs = ad.State.Jobs
		}
		r.lock.RUnlock()

		for _, j := range jobs {
			state, err := r.agentClient.TerminateJob(context.Background(), j.Id, rpc.JobTerminateReason_TERMINATION_REQUESTED)
			if err != nil {
				continue
			}
			if state.ParticipantIdentity != "" {
				r.lock.RLock()
				agentJob := r.agentParticpants[livekit.ParticipantIdentity(state.ParticipantIdentity)]
				p := r.participants[livekit.ParticipantIdentity(state.ParticipantIdentity)]
				r.lock.RUnlock()

				if p != nil {
					if agentJob != nil {
						err := agentJob.waitForParticipantLeaving()
						if err == ErrJobShutdownTimeout {
							r.logger.Infow("Agent Worker did not disconnect after 3s")
						}
					}
					r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonServiceRequestRemoveParticipant)
				}
			}
			r.lock.Lock()
			j.State = state
			r.lock.Unlock()
		}
	}()

	return ad.AgentDispatch, nil
}

func (r *Room) OnRoomUpdated(f func()) {
	r.onRoomUpdated = f
}

func (r *Room) onSimulateScenario(participant types.LocalParticipant, simulateScenario *livekit.SimulateScenario) error {
	switch scenario := simulateScenario.Scenario.(type) {
	case *livekit.SimulateScenario_SpeakerUpdate:
		r.logger.Infow("simulating speaker update", "participant", participant.Identity(), "duration", scenario.SpeakerUpdate)
		go func() {
			<-time.After(time.Duration(scenario.SpeakerUpdate) * time.Second)
			r.sendSpeakerChanges([]*livekit.SpeakerInfo{{
				Sid:    string(participant.ID()),
				Active: false,
				Level:  0,
			}})
		}()
		r.sendSpeakerChanges([]*livekit.SpeakerInfo{{
			Sid:    string(participant.ID()),
			Active: true,
			Level:  0.9,
		}})
	case *livekit.SimulateScenario_Migration:
		r.logger.Infow("simulating migration", "participant", participant.Identity())
		// drop participant without necessarily cleaning up
		if err := participant.Close(false, types.ParticipantCloseReasonSimulateMigration, true); err != nil {
			return err
		}
	case *livekit.SimulateScenario_NodeFailure:
		r.logger.Infow("simulating node failure", "participant", participant.Identity())
		// drop participant without necessarily cleaning up
		if err := participant.Close(false, types.ParticipantCloseReasonSimulateNodeFailure, true); err != nil {
			return err
		}
	case *livekit.SimulateScenario_ServerLeave:
		r.logger.Infow("simulating server leave", "participant", participant.Identity())
		if err := participant.Close(true, types.ParticipantCloseReasonSimulateServerLeave, false); err != nil {
			return err
		}
	case *livekit.SimulateScenario_SwitchCandidateProtocol:
		r.logger.Infow("simulating switch candidate protocol", "participant", participant.Identity())
		participant.ICERestart(&livekit.ICEConfig{
			PreferenceSubscriber: livekit.ICECandidateType(scenario.SwitchCandidateProtocol),
			PreferencePublisher:  livekit.ICECandidateType(scenario.SwitchCandidateProtocol),
		})
	case *livekit.SimulateScenario_SubscriberBandwidth:
		if scenario.SubscriberBandwidth > 0 {
			r.logger.Infow("simulating subscriber bandwidth start", "participant", participant.Identity(), "bandwidth", scenario.SubscriberBandwidth)
		} else {
			r.logger.Infow("simulating subscriber bandwidth end", "participant", participant.Identity())
		}
		participant.SetSubscriberChannelCapacity(scenario.SubscriberBandwidth)
	case *livekit.SimulateScenario_DisconnectSignalOnResume:
		participant.GetLogger().Infow("simulating disconnect signal on resume")
		r.simulationLock.Lock()
		r.disconnectSignalOnResumeParticipants[participant.Identity()] = time.Now().Add(simulateDisconnectSignalTimeout)
		r.simulationLock.Unlock()
	case *livekit.SimulateScenario_DisconnectSignalOnResumeNoMessages:
		participant.GetLogger().Infow("simulating disconnect signal on resume before sending any response messages")
		r.simulationLock.Lock()
		r.disconnectSignalOnResumeNoMessagesParticipants[participant.Identity()] = &disconnectSignalOnResumeNoMessages{
			expiry: time.Now().Add(simulateDisconnectSignalTimeout),
		}
		r.simulationLock.Unlock()
	}
	return nil
}

// checks if participant should be autosubscribed to new tracks, assumes lock is already acquired
func (r *Room) autoSubscribe(participant types.LocalParticipant) bool {
	opts := r.participantOpts[participant.Identity()]
	// default to true if no options are set
	if opts != nil && !opts.AutoSubscribe {
		return false
	}
	return true
}

func (r *Room) createJoinResponseLocked(
	participant types.LocalParticipant,
	iceServers []*livekit.ICEServer,
) *livekit.JoinResponse {
	iceConfig := participant.GetICEConfig()
	hasICEFallback := iceConfig.GetPreferencePublisher() != livekit.ICECandidateType_ICT_NONE || iceConfig.GetPreferenceSubscriber() != livekit.ICECandidateType_ICT_NONE
	return &livekit.JoinResponse{
		Room:        r.ToProto(),
		Participant: participant.ToProto(),
		OtherParticipants: GetOtherParticipantInfo(
			participant,
			false, // isMigratingIn
			toParticipants(maps.Values(r.participants)),
			false, // skipSubscriberBroadcast
		),
		IceServers: iceServers,
		// indicates both server and client support subscriber as primary
		SubscriberPrimary:   participant.SubscriberAsPrimary(),
		ClientConfiguration: participant.GetClientConfiguration(),
		// sane defaults for ping interval & timeout
		PingInterval:         PingIntervalSeconds,
		PingTimeout:          PingTimeoutSeconds,
		ServerInfo:           r.serverInfo,
		ServerVersion:        r.serverInfo.Version,
		ServerRegion:         r.serverInfo.Region,
		SifTrailer:           r.trailer,
		EnabledPublishCodecs: participant.GetEnabledPublishCodecs(),
		FastPublish:          participant.CanPublish() && !hasICEFallback,
	}
}

// a ParticipantImpl in the room added a new track, subscribe other participants to it
func (r *Room) onTrackPublished(participant types.LocalParticipant, track types.MediaTrack) {
	// publish participant update, since track state is changed
	r.broadcastParticipantState(participant, broadcastOptions{skipSource: true})

	r.lock.RLock()
	// subscribe all existing participants to this MediaTrack
	for _, existingParticipant := range r.participants {
		if existingParticipant == participant {
			// skip publishing participant
			continue
		}
		if existingParticipant.State() != livekit.ParticipantInfo_ACTIVE {
			// not fully joined. don't subscribe yet
			continue
		}
		if !r.autoSubscribe(existingParticipant) {
			continue
		}

		existingParticipant.GetLogger().Debugw(
			"subscribing to new track",
			"publisher", participant.Identity(),
			"publisherID", participant.ID(),
			"trackID", track.ID(),
		)
		existingParticipant.SubscribeToTrack(track.ID(), false)
	}
	onParticipantChanged := r.onParticipantChanged
	r.lock.RUnlock()

	if onParticipantChanged != nil {
		onParticipantChanged(participant)
	}

	r.trackManager.AddTrack(track, participant.Identity(), participant.ID())

	// launch jobs
	r.lock.Lock()
	hasPublished := r.hasPublished[participant.Identity()]
	r.hasPublished[participant.Identity()] = true
	r.lock.Unlock()

	if !hasPublished {
		r.lock.RLock()
		r.launchTargetAgents(maps.Values(r.agentDispatches), participant, livekit.JobType_JT_PUBLISHER)
		r.lock.RUnlock()
		if r.internal != nil && r.internal.ParticipantEgress != nil {
			go func() {
				if err := StartParticipantEgress(
					context.Background(),
					r.egressLauncher,
					r.telemetry,
					r.internal.ParticipantEgress,
					participant.Identity(),
					r.Name(),
					r.ID(),
				); err != nil {
					r.logger.Errorw("failed to launch participant egress", err)
				}
			}()
		}
	}
	if participant.Kind() != livekit.ParticipantInfo_EGRESS && r.internal != nil && r.internal.TrackEgress != nil {
		go func() {
			if err := StartTrackEgress(
				context.Background(),
				r.egressLauncher,
				r.telemetry,
				r.internal.TrackEgress,
				track,
				r.Name(),
				r.ID(),
			); err != nil {
				r.logger.Errorw("failed to launch track egress", err)
			}
		}()
	}
}

func (r *Room) onTrackUpdated(p types.LocalParticipant, _ types.MediaTrack) {
	// send track updates to everyone, especially if track was updated by admin
	r.broadcastParticipantState(p, broadcastOptions{})
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onTrackUnpublished(p types.LocalParticipant, track types.MediaTrack) {
	r.trackManager.RemoveTrack(track)
	if !p.IsClosed() {
		r.broadcastParticipantState(p, broadcastOptions{skipSource: true})
	}
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onParticipantUpdate(p types.LocalParticipant) {
	r.protoProxy.MarkDirty(false)
	// immediately notify when permissions or metadata changed
	r.broadcastParticipantState(p, broadcastOptions{immediate: true})
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onDataPacket(source types.LocalParticipant, kind livekit.DataPacket_Kind, dp *livekit.DataPacket) {
	if kind == livekit.DataPacket_RELIABLE && source != nil && dp.GetSequence() > 0 {
		data, err := proto.Marshal(dp)
		if err != nil {
			r.logger.Errorw("failed to marshal data packet for cache", err, "participant", source.Identity(), "seq", dp.GetSequence())
			return
		}
		r.dataMessageCache.Add(&types.DataMessageCache{
			SenderID:       source.ID(),
			Seq:            dp.GetSequence(),
			Data:           data,
			DestIdentities: livekit.StringsAsIDs[livekit.ParticipantIdentity](dp.DestinationIdentities),
		}, len(data))
	}
	BroadcastDataPacketForRoom(r, source, kind, dp, r.logger)
}

func (r *Room) onDataMessage(source types.LocalParticipant, data []byte) {
	BroadcastDataMessageForRoom(r, source, data, r.logger)
}

func (r *Room) onMetrics(source types.Participant, dp *livekit.DataPacket) {
	BroadcastMetricsForRoom(r, source, dp, r.logger)
}

func (r *Room) onUpdateSubscriptions(
	participant types.LocalParticipant,
	trackIDs []livekit.TrackID,
	participantTracks []*livekit.ParticipantTracks,
	subscribe bool,
) {
	r.UpdateSubscriptions(participant, trackIDs, participantTracks, subscribe)
}

func (r *Room) UpdateSubscriptions(
	participant types.LocalParticipant,
	trackIDs []livekit.TrackID,
	participantTracks []*livekit.ParticipantTracks,
	subscribe bool,
) {
	// handle subscription changes
	for _, trackID := range trackIDs {
		if subscribe {
			participant.SubscribeToTrack(trackID, false)
		} else {
			participant.UnsubscribeFromTrack(trackID)
		}
	}

	for _, pt := range participantTracks {
		for _, trackID := range livekit.StringsAsIDs[livekit.TrackID](pt.TrackSids) {
			if subscribe {
				participant.SubscribeToTrack(trackID, false)
			} else {
				participant.UnsubscribeFromTrack(trackID)
			}
		}
	}
}

func (r *Room) onLeave(p types.LocalParticipant, reason types.ParticipantCloseReason) {
	r.RemoveParticipant(p.Identity(), p.ID(), reason)
}

func (r *Room) RemoveParticipant(
	identity livekit.ParticipantIdentity,
	pID livekit.ParticipantID,
	reason types.ParticipantCloseReason,
) {
	r.lock.Lock()
	p, ok := r.participants[identity]
	if !ok {
		r.lock.Unlock()
		return
	}

	if pID != "" && p.ID() != pID {
		// participant session has been replaced
		r.lock.Unlock()
		return
	}

	agentJob := r.agentParticpants[identity]

	delete(r.participants, identity)
	delete(r.participantOpts, identity)
	delete(r.participantRequestSources, identity)
	delete(r.hasPublished, identity)
	delete(r.agentParticpants, identity)
	if !p.Hidden() {
		r.protoRoom.NumParticipants--
	}

	immediateChange := false
	if p.IsRecorder() {
		activeRecording := false
		for _, op := range r.participants {
			if op.IsRecorder() {
				activeRecording = true
				break
			}
		}

		if r.protoRoom.ActiveRecording != activeRecording {
			r.protoRoom.ActiveRecording = activeRecording
			immediateChange = true
		}
	}
	r.lock.Unlock()
	r.protoProxy.MarkDirty(immediateChange)

	if !p.HasConnected() {
		fields := append(
			connectionDetailsFields(p.GetICEConnectionInfo()),
			"reason", reason.String(),
			"clientInfo", logger.Proto(sutils.ClientInfoWithoutAddress(p.GetClientInfo())),
		)
		p.GetLogger().Infow("removing participant without connection", fields...)
	}

	// send broadcast only if it's not already closed
	sendUpdates := !p.IsDisconnected()

	// remove all published tracks
	for _, t := range p.GetPublishedTracks() {
		p.RemovePublishedTrack(t, false)
		r.trackManager.RemoveTrack(t)
	}

	if agentJob != nil {
		agentJob.participantLeft()

		go func() {
			_, err := r.agentClient.TerminateJob(context.Background(), agentJob.Id, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
			if err != nil {
				r.logger.Infow("failed sending TerminateJob RPC", "error", err, "jobID", agentJob.Id, "participant", identity)
			}
		}()
	}

	p.OnTrackUpdated(nil)
	p.OnTrackPublished(nil)
	p.OnTrackUnpublished(nil)
	p.OnStateChange(nil)
	p.OnSubscriberReady(nil)
	p.OnParticipantUpdate(nil)
	p.OnDataPacket(nil)
	p.OnDataMessage(nil)
	p.OnMetrics(nil)
	p.OnSubscribeStatusChanged(nil)
	p.OnUpdateSubscriptions(nil)
	p.OnUpdateSubscriptionPermission(nil)
	p.OnSyncState(nil)
	p.OnSimulateScenario(nil)
	p.OnLeave(nil)

	// close participant as well
	_ = p.Close(true, reason, false)

	r.leftAt.Store(time.Now().Unix())

	if sendUpdates {
		if r.onParticipantChanged != nil {
			r.onParticipantChanged(p)
		}
		r.broadcastParticipantState(p, broadcastOptions{skipSource: true})
	}
}

func (r *Room) subscribeToExistingTracks(p types.LocalParticipant, isSync bool) {
	r.lock.RLock()
	shouldSubscribe := r.autoSubscribe(p)
	r.lock.RUnlock()
	if !shouldSubscribe {
		return
	}

	var trackIDs []livekit.TrackID
	for _, op := range r.GetParticipants() {
		if p.ID() == op.ID() {
			// don't send to itself
			continue
		}

		// subscribe to all
		for _, track := range op.GetPublishedTracks() {
			trackIDs = append(trackIDs, track.ID())
			p.SubscribeToTrack(track.ID(), isSync)
		}
	}
	if len(trackIDs) > 0 {
		p.GetLogger().Debugw("subscribed participant to existing tracks", "trackID", trackIDs)
	}
}

// broadcast an update about participant p
func (r *Room) broadcastParticipantState(p types.LocalParticipant, opts broadcastOptions) {
	pi := p.ToProto()

	// send it to the same participant immediately
	selfSent := false
	if !opts.skipSource {
		defer func() {
			if selfSent {
				return
			}

			err := p.SendParticipantUpdate([]*livekit.ParticipantInfo{pi})
			if err != nil {
				p.GetLogger().Errorw("could not send update to participant", err)
			}
		}()
	}

	if p.Hidden() {
		// hidden participant updates are sent only to the hidden participant itself,
		// these could be things like metadata update
		return
	}

	r.batchedUpdatesMu.Lock()
	updates := PushAndDequeueUpdates(
		pi,
		p.CloseReason(),
		opts.immediate,
		r.GetParticipant(livekit.ParticipantIdentity(pi.Identity)),
		r.batchedUpdates,
	)
	r.batchedUpdatesMu.Unlock()
	if len(updates) != 0 {
		selfSent = true
		SendParticipantUpdates(updates, r.GetParticipants(), r.roomConfig.UpdateBatchTargetSize)
	}
}

// for protocol 3, send only changed updates
func (r *Room) sendSpeakerChanges(speakers []*livekit.SpeakerInfo) {
	for _, p := range r.GetParticipants() {
		if p.ProtocolVersion().SupportsSpeakerChanged() {
			_ = p.SendSpeakerUpdate(speakers, false)
		}
	}
}

func (r *Room) updateProto() *livekit.Room {
	r.lock.RLock()
	room := utils.CloneProto(r.protoRoom)
	r.lock.RUnlock()

	room.NumPublishers = 0
	room.NumParticipants = 0
	for _, p := range r.GetParticipants() {
		if !p.IsDependent() {
			room.NumParticipants++
		}
		if p.IsPublisher() {
			room.NumPublishers++
		}
	}

	return room
}

func (r *Room) changeUpdateWorker() {
	subTicker := time.NewTicker(subscriberUpdateInterval)
	defer subTicker.Stop()

	cleanDataMessageTicker := time.NewTicker(dataMessageCacheTTL)

	for !r.IsClosed() {
		select {
		case <-r.closed:
			return
		case <-r.protoProxy.Updated():
			if r.onRoomUpdated != nil {
				r.onRoomUpdated()
			}
			r.sendRoomUpdate()
		case <-subTicker.C:
			r.batchedUpdatesMu.Lock()
			if len(r.batchedUpdates) == 0 {
				r.batchedUpdatesMu.Unlock()
				continue
			}
			updatesMap := r.batchedUpdates
			r.batchedUpdates = make(map[livekit.ParticipantIdentity]*ParticipantUpdate)
			r.batchedUpdatesMu.Unlock()

			SendParticipantUpdates(maps.Values(updatesMap), r.GetParticipants(), r.roomConfig.UpdateBatchTargetSize)

		case <-cleanDataMessageTicker.C:
			r.dataMessageCache.Prune()
		}
	}
}

func (r *Room) audioUpdateWorker() {
	lastActiveMap := make(map[livekit.ParticipantID]*livekit.SpeakerInfo)
	for {
		if r.IsClosed() {
			return
		}

		activeSpeakers := r.GetActiveSpeakers()
		changedSpeakers := make([]*livekit.SpeakerInfo, 0, len(activeSpeakers))
		nextActiveMap := make(map[livekit.ParticipantID]*livekit.SpeakerInfo, len(activeSpeakers))
		for _, speaker := range activeSpeakers {
			prev := lastActiveMap[livekit.ParticipantID(speaker.Sid)]
			if prev == nil || prev.Level != speaker.Level {
				changedSpeakers = append(changedSpeakers, speaker)
			}
			nextActiveMap[livekit.ParticipantID(speaker.Sid)] = speaker
		}

		// changedSpeakers need to include previous speakers that are no longer speaking
		for sid, speaker := range lastActiveMap {
			if nextActiveMap[sid] == nil {
				inactiveSpeaker := utils.CloneProto(speaker)
				inactiveSpeaker.Level = 0
				inactiveSpeaker.Active = false
				changedSpeakers = append(changedSpeakers, inactiveSpeaker)
			}
		}

		// see if an update is needed
		if len(changedSpeakers) > 0 {
			r.sendSpeakerChanges(changedSpeakers)
		}

		lastActiveMap = nextActiveMap

		time.Sleep(time.Duration(r.audioConfig.UpdateInterval) * time.Millisecond)
	}
}

func (r *Room) connectionQualityWorker() {
	ticker := time.NewTicker(connectionquality.UpdateInterval)
	defer ticker.Stop()

	prevConnectionInfos := make(map[livekit.ParticipantID]*livekit.ConnectionQualityInfo)
	// send updates to only users that are subscribed to each other
	for !r.IsClosed() {
		<-ticker.C

		participants := r.GetParticipants()
		nowConnectionInfos := make(map[livekit.ParticipantID]*livekit.ConnectionQualityInfo, len(participants))

		for _, p := range participants {
			if p.State() != livekit.ParticipantInfo_ACTIVE {
				continue
			}

			if q := p.GetConnectionQuality(); q != nil {
				nowConnectionInfos[p.ID()] = q
			}
		}

		// send an update if there is a change
		//   - new participant
		//   - quality change
		// NOTE: participant leaving is explicitly omitted as `leave` signal notifies that a participant is not in the room anymore
		sendUpdate := false
		for _, p := range participants {
			pID := p.ID()
			prevInfo, prevOk := prevConnectionInfos[pID]
			nowInfo, nowOk := nowConnectionInfos[pID]
			if !nowOk {
				// participant is not ACTIVE any more
				continue
			}
			if !prevOk || nowInfo.Quality != prevInfo.Quality {
				// new entrant OR change in quality
				sendUpdate = true
				break
			}
		}

		if !sendUpdate {
			prevConnectionInfos = nowConnectionInfos
			continue
		}

		maybeAddToUpdate := func(pID livekit.ParticipantID, update *livekit.ConnectionQualityUpdate) {
			if nowInfo, nowOk := nowConnectionInfos[pID]; nowOk {
				update.Updates = append(update.Updates, nowInfo)
			}
		}

		for _, op := range participants {
			if !op.ProtocolVersion().SupportsConnectionQuality() || op.State() != livekit.ParticipantInfo_ACTIVE {
				continue
			}
			update := &livekit.ConnectionQualityUpdate{}

			// send to user itself
			maybeAddToUpdate(op.ID(), update)

			// add connection quality of other participants its subscribed to
			for _, sid := range op.GetSubscribedParticipants() {
				maybeAddToUpdate(sid, update)
			}
			if len(update.Updates) == 0 {
				// no change
				continue
			}
			if err := op.SendConnectionQualityUpdate(update); err != nil {
				r.logger.Warnw("could not send connection quality update", err,
					"participant", op.Identity())
			}
		}

		prevConnectionInfos = nowConnectionInfos
	}
}

func (r *Room) simulationCleanupWorker() {
	for {
		if r.IsClosed() {
			return
		}

		now := time.Now()
		r.simulationLock.Lock()
		for identity, timeout := range r.disconnectSignalOnResumeParticipants {
			if now.After(timeout) {
				delete(r.disconnectSignalOnResumeParticipants, identity)
			}
		}

		for identity, state := range r.disconnectSignalOnResumeNoMessagesParticipants {
			if now.After(state.expiry) {
				delete(r.disconnectSignalOnResumeNoMessagesParticipants, identity)
			}
		}
		r.simulationLock.Unlock()

		time.Sleep(10 * time.Second)
	}
}

func (r *Room) launchRoomAgents(ads []*agentDispatch) {
	if r.agentClient == nil {
		return
	}

	for _, ad := range ads {
		done := ad.jobsLaunching()

		go func() {
			inc := r.agentClient.LaunchJob(context.Background(), &agent.JobRequest{
				JobType:    livekit.JobType_JT_ROOM,
				Room:       r.ToProto(),
				Metadata:   ad.Metadata,
				AgentName:  ad.AgentName,
				DispatchId: ad.Id,
			})
			r.handleNewJobs(ad.AgentDispatch, inc)
			done()
		}()
	}
}

func (r *Room) launchTargetAgents(ads []*agentDispatch, p types.Participant, jobType livekit.JobType) {
	if p == nil || p.IsDependent() || r.agentClient == nil {
		return
	}

	for _, ad := range ads {
		done := ad.jobsLaunching()

		go func() {
			inc := r.agentClient.LaunchJob(context.Background(), &agent.JobRequest{
				JobType:     jobType,
				Room:        r.ToProto(),
				Participant: p.ToProto(),
				Metadata:    ad.Metadata,
				AgentName:   ad.AgentName,
				DispatchId:  ad.Id,
			})
			r.handleNewJobs(ad.AgentDispatch, inc)
			done()
		}()
	}
}

func (r *Room) handleNewJobs(ad *livekit.AgentDispatch, inc *sutils.IncrementalDispatcher[*livekit.Job]) {
	inc.ForEach(func(job *livekit.Job) {
		r.agentStore.StoreAgentJob(context.Background(), job)
		r.lock.Lock()
		ad.State.Jobs = append(ad.State.Jobs, job)
		if job.State != nil && job.State.ParticipantIdentity != "" {
			r.agentParticpants[livekit.ParticipantIdentity(job.State.ParticipantIdentity)] = newAgentJob(job)
		}
		r.lock.Unlock()
	})
}

func (r *Room) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"Name":      r.protoRoom.Name,
		"Sid":       r.protoRoom.Sid,
		"CreatedAt": r.protoRoom.CreationTime,
	}

	participants := r.GetParticipants()
	participantInfo := make(map[string]interface{})
	for _, p := range participants {
		participantInfo[string(p.Identity())] = p.DebugInfo()
	}
	info["Participants"] = participantInfo

	return info
}

func (r *Room) createAgentDispatch(dispatch *livekit.AgentDispatch) (*agentDispatch, error) {
	dispatch.State = &livekit.AgentDispatchState{
		CreatedAt: time.Now().UnixNano(),
	}
	ad := newAgentDispatch(dispatch)

	r.lock.RLock()
	r.agentDispatches[ad.Id] = ad
	r.lock.RUnlock()
	if r.agentStore != nil {
		err := r.agentStore.StoreAgentDispatch(context.Background(), ad.AgentDispatch)
		if err != nil {
			return nil, err
		}
	}

	return ad, nil
}

func (r *Room) createAgentDispatchFromParams(agentName string, metadata string) (*agentDispatch, error) {
	return r.createAgentDispatch(&livekit.AgentDispatch{
		Id:        guid.New(guid.AgentDispatchPrefix),
		AgentName: agentName,
		Metadata:  metadata,
		Room:      r.protoRoom.Name,
	})
}

func (r *Room) createAgentDispatchesFromRoomAgent() {
	if r.internal == nil {
		return
	}

	roomDisp := r.internal.AgentDispatches
	if len(roomDisp) == 0 {
		// Backward compatibility: by default, start any agent in the empty JobName
		roomDisp = []*livekit.RoomAgentDispatch{{}}
	}

	for _, ag := range roomDisp {
		_, err := r.createAgentDispatchFromParams(ag.AgentName, ag.Metadata)
		if err != nil {
			r.logger.Warnw("failed storing room dispatch", err)
		}
	}
}

func (r *Room) IsDataMessageUserPacketDuplicate(up *livekit.UserPacket) bool {
	return r.userPacketDeduper.IsDuplicate(up)
}

func (r *Room) GetCachedReliableDataMessage(seqs map[livekit.ParticipantID]uint32) []*types.DataMessageCache {
	msgs := make([]*types.DataMessageCache, 0, len(seqs)*10)
	for _, msg := range r.dataMessageCache.Get() {
		seq, ok := seqs[msg.SenderID]
		if ok && msg.Seq >= seq {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

// ------------------------------------------------------------

func BroadcastDataPacketForRoom(
	r types.Room,
	source types.LocalParticipant,
	kind livekit.DataPacket_Kind,
	dp *livekit.DataPacket,
	logger logger.Logger,
) {
	dp.Kind = kind // backward compatibility
	dest := dp.GetUser().GetDestinationSids()
	if u := dp.GetUser(); u != nil {
		if r.IsDataMessageUserPacketDuplicate(u) {
			logger.Infow("dropping duplicate data message", "nonce", u.Nonce)
			return
		}
		if len(dp.DestinationIdentities) == 0 {
			dp.DestinationIdentities = u.DestinationIdentities
		} else {
			u.DestinationIdentities = dp.DestinationIdentities
		}
		if dp.ParticipantIdentity != "" {
			u.ParticipantIdentity = dp.ParticipantIdentity
		} else {
			dp.ParticipantIdentity = u.ParticipantIdentity
		}
	}
	destIdentities := dp.DestinationIdentities

	participants := r.GetLocalParticipants()
	capacity := len(destIdentities)
	if capacity == 0 {
		capacity = len(dest)
	}
	if capacity == 0 {
		capacity = len(participants)
	}
	destParticipants := make([]types.LocalParticipant, 0, capacity)

	var dpData []byte
	for _, op := range participants {
		if source != nil && op.ID() == source.ID() {
			continue
		}
		if len(dest) > 0 || len(destIdentities) > 0 {
			if !slices.Contains(dest, string(op.ID())) && !slices.Contains(destIdentities, string(op.Identity())) {
				continue
			}
		}
		if dpData == nil {
			var err error
			dpData, err = proto.Marshal(dp)
			if err != nil {
				logger.Errorw("failed to marshal data packet", err)
				return
			}
		}
		destParticipants = append(destParticipants, op)
	}

	utils.ParallelExec(destParticipants, dataForwardLoadBalanceThreshold, 1, func(op types.LocalParticipant) {
		op.SendDataMessage(kind, dpData, livekit.ParticipantID(dp.GetParticipantSid()), dp.GetSequence())
	})
}

func BroadcastDataMessageForRoom(r types.Room, source types.LocalParticipant, data []byte, logger logger.Logger) {
	utils.ParallelExec(r.GetLocalParticipants(), dataForwardLoadBalanceThreshold, 1, func(op types.LocalParticipant) {
		if source != nil && op.ID() == source.ID() {
			return
		}

		op.SendDataMessageUnlabeled(data, false, source.Identity())
	})
}

func BroadcastMetricsForRoom(r types.Room, source types.Participant, dp *livekit.DataPacket, logger logger.Logger) {
	switch payload := dp.Value.(type) {
	case *livekit.DataPacket_Metrics:
		utils.ParallelExec(r.GetLocalParticipants(), dataForwardLoadBalanceThreshold, 1, func(op types.LocalParticipant) {
			// echoing back to sender too
			op.HandleMetrics(source.ID(), payload.Metrics)
		})
	default:
	}
}

func IsCloseNotifySkippable(closeReason types.ParticipantCloseReason) bool {
	return closeReason == types.ParticipantCloseReasonDuplicateIdentity
}

func IsParticipantExemptFromTrackPermissionsRestrictions(p types.LocalParticipant) bool {
	// egress/recorder participants bypass permissions as auto-egress does not
	// have enough context to check permissions
	return p.IsRecorder()
}

func CompareParticipant(pi1 *livekit.ParticipantInfo, pi2 *livekit.ParticipantInfo) int {
	if pi1.JoinedAt != pi2.JoinedAt {
		if pi1.JoinedAt < pi2.JoinedAt {
			return -1
		} else {
			return 1
		}
	}

	if pi1.JoinedAtMs != 0 && pi2.JoinedAtMs != 0 && pi1.JoinedAtMs != pi2.JoinedAtMs {
		if pi1.JoinedAtMs < pi2.JoinedAtMs {
			return -1
		} else {
			return 1
		}
	}

	// all join times being equal, it is not possible to really know which one is newer,
	// pick the higher pID to be consistent
	if pi1.Sid != pi2.Sid {
		if pi1.Sid < pi2.Sid {
			return -1
		} else {
			return 1
		}
	}

	return 0
}

type ParticipantUpdate struct {
	ParticipantInfo         *livekit.ParticipantInfo
	IsSynthesizedDisconnect bool
	CloseReason             types.ParticipantCloseReason
}

// push a participant update for batched broadcast, optionally returning immediate updates to broadcast.
// it handles the following scenarios
// * subscriber-only updates will be queued for batch updates
// * publisher & immediate updates will be returned without queuing
// * when the SID changes, it will return both updates, with the earlier participant set to disconnected
func PushAndDequeueUpdates(
	pi *livekit.ParticipantInfo,
	closeReason types.ParticipantCloseReason,
	isImmediate bool,
	existingParticipant types.Participant,
	cache map[livekit.ParticipantIdentity]*ParticipantUpdate,
) []*ParticipantUpdate {
	var updates []*ParticipantUpdate
	identity := livekit.ParticipantIdentity(pi.Identity)
	existing := cache[identity]
	shouldSend := isImmediate || pi.IsPublisher

	if existing != nil {
		if pi.Sid == existing.ParticipantInfo.Sid {
			// same participant session
			if pi.Version < existing.ParticipantInfo.Version {
				// out of order update
				return nil
			}
		} else {
			// different participant sessions
			if CompareParticipant(existing.ParticipantInfo, pi) < 0 {
				// existing is older, synthesize a DISCONNECT for older and
				// send immediately along with newer session to signal switch
				shouldSend = true
				existing.ParticipantInfo.State = livekit.ParticipantInfo_DISCONNECTED
				existing.IsSynthesizedDisconnect = true
				updates = append(updates, existing)
			} else {
				// older session update, newer session has already become active, so nothing to do
				return nil
			}
		}
	} else {
		if existingParticipant != nil {
			epi := existingParticipant.ToProto()
			if CompareParticipant(epi, pi) > 0 {
				// older session update, newer session has already become active, so nothing to do
				return nil
			}
		}
	}

	if shouldSend {
		// include any queued update, and return
		delete(cache, identity)
		updates = append(updates, &ParticipantUpdate{ParticipantInfo: pi, CloseReason: closeReason})
	} else {
		// enqueue for batch
		cache[identity] = &ParticipantUpdate{ParticipantInfo: pi, CloseReason: closeReason}
	}

	return updates
}

func SendParticipantUpdates(updates []*ParticipantUpdate, participants []types.LocalParticipant, batchTargetSize int) {
	if len(updates) == 0 {
		return
	}

	// For filtered updates, skip
	// 1. synthesized DISCONNECT - this happens on SID change
	// 2. close reasons of DUPLICATE_IDENTITY/STALE  - A newer session for that identity exists.
	//
	// Filtered updates are used with clients that can handle identity based reconnect and hence those
	// conditions can be skipped.
	var filteredUpdates []*livekit.ParticipantInfo
	for _, update := range updates {
		if update.IsSynthesizedDisconnect || IsCloseNotifySkippable(update.CloseReason) {
			continue
		}
		filteredUpdates = append(filteredUpdates, update.ParticipantInfo)
	}

	var fullUpdates []*livekit.ParticipantInfo
	for _, update := range updates {
		fullUpdates = append(fullUpdates, update.ParticipantInfo)
	}

	filteredUpdateChunks := ChunkProtoBatch(filteredUpdates, batchTargetSize)
	fullUpdateChunks := ChunkProtoBatch(fullUpdates, batchTargetSize)

	for _, op := range participants {
		updateChunks := fullUpdateChunks
		if op.ProtocolVersion().SupportsIdentityBasedReconnection() {
			updateChunks = filteredUpdateChunks
		}
		for _, chunk := range updateChunks {
			if err := op.SendParticipantUpdate(chunk); err != nil {
				op.GetLogger().Errorw("could not send update to participant", err)
				break
			}
		}
	}
}

// GetOtherParticipantInfo returns ParticipantInfo for everyone in the room except for the participant identified by lp.Identity()
func GetOtherParticipantInfo(
	lp types.LocalParticipant,
	isMigratingIn bool,
	allParticipants []types.Participant,
	skipSubscriberBroadcast bool,
) []*livekit.ParticipantInfo {
	var lpIdentity livekit.ParticipantIdentity
	if lp != nil {
		lpIdentity = lp.Identity()
	}

	pInfos := make([]*livekit.ParticipantInfo, 0, len(allParticipants))
	for _, op := range allParticipants {
		if !(skipSubscriberBroadcast && op.CanSkipBroadcast()) &&
			!op.Hidden() &&
			op.Identity() != lpIdentity &&
			!isMigratingIn {
			pInfos = append(pInfos, op.ToProto())
		}
	}

	return pInfos
}

func connectionDetailsFields(infos []*types.ICEConnectionInfo) []interface{} {
	var fields []interface{}
	connectionType := types.ICEConnectionTypeUnknown
	for _, info := range infos {
		candidates := make([]string, 0, len(info.Remote)+len(info.Local))
		for _, c := range info.Local {
			cStr := "[local]"
			if c.SelectedOrder != 0 {
				cStr += fmt.Sprintf("[selected:%d]", c.SelectedOrder)
			} else if c.Filtered {
				cStr += "[filtered]"
			}
			if c.Trickle {
				cStr += "[trickle]"
			}
			cStr += " " + c.Local.String()
			candidates = append(candidates, cStr)
		}
		for _, c := range info.Remote {
			cStr := "[remote]"
			if c.SelectedOrder != 0 {
				cStr += fmt.Sprintf("[selected:%d]", c.SelectedOrder)
			} else if c.Filtered {
				cStr += "[filtered]"
			}
			if c.Trickle {
				cStr += "[trickle]"
			}
			cStr += " " + fmt.Sprintf("%s %s %s:%d", c.Remote.NetworkType(), c.Remote.Type(), MaybeTruncateIP(c.Remote.Address()), c.Remote.Port())
			if relatedAddress := c.Remote.RelatedAddress(); relatedAddress != nil {
				relatedAddr := MaybeTruncateIP(relatedAddress.Address)
				if relatedAddr != "" {
					cStr += " " + fmt.Sprintf(" related %s:%d", relatedAddr, relatedAddress.Port)
				}
			}
			candidates = append(candidates, cStr)
		}
		if len(candidates) > 0 {
			fields = append(fields, fmt.Sprintf("%sCandidates", strings.ToLower(info.Transport.String())), candidates)
		}
		if info.Type != types.ICEConnectionTypeUnknown {
			connectionType = info.Type
		}
	}
	fields = append(fields, "connectionType", connectionType)
	return fields
}

func toParticipants(lps []types.LocalParticipant) []types.Participant {
	participants := make([]types.Participant, len(lps))
	for idx, lp := range lps {
		participants[idx] = lp
	}
	return participants
}
