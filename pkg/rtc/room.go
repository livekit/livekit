package rtc

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const (
	DefaultEmptyTimeout       = 5 * 60 // 5m
	DefaultRoomDepartureGrace = 20
	AudioLevelQuantization    = 8 // ideally power of 2 to minimize float decimal
	invAudioLevelQuantization = 1.0 / AudioLevelQuantization
	subscriberUpdateInterval  = 3 * time.Second
)

type broadcastOptions struct {
	skipSource bool
	immediate  bool
}

type Room struct {
	lock sync.RWMutex

	protoRoom *livekit.Room
	internal  *livekit.RoomInternal
	Logger    logger.Logger

	config         WebRTCConfig
	audioConfig    *config.AudioConfig
	serverInfo     *livekit.ServerInfo
	telemetry      telemetry.TelemetryService
	egressLauncher EgressLauncher

	// map of identity -> Participant
	participants    map[livekit.ParticipantIdentity]types.LocalParticipant
	participantOpts map[livekit.ParticipantIdentity]*ParticipantOptions
	bufferFactory   *buffer.Factory

	// batch update participant info for non-publishers
	batchedUpdates   map[livekit.ParticipantIdentity]*livekit.ParticipantInfo
	batchedUpdatesMu sync.Mutex

	// time the first participant joined the room
	joinedAt atomic.Int64
	holds    atomic.Int32
	// time that the last participant left the room
	leftAt atomic.Int64
	closed chan struct{}

	onParticipantChanged func(p types.LocalParticipant)
	onMetadataUpdate     func(metadata string)
	onClose              func()
}

type ParticipantOptions struct {
	AutoSubscribe bool
}

func NewRoom(
	room *livekit.Room,
	internal *livekit.RoomInternal,
	config WebRTCConfig,
	audioConfig *config.AudioConfig,
	serverInfo *livekit.ServerInfo,
	telemetry telemetry.TelemetryService,
	egressLauncher EgressLauncher,
) *Room {
	r := &Room{
		protoRoom:       proto.Clone(room).(*livekit.Room),
		internal:        internal,
		Logger:          LoggerWithRoom(logger.GetDefaultLogger(), livekit.RoomName(room.Name), livekit.RoomID(room.Sid)),
		config:          config,
		audioConfig:     audioConfig,
		telemetry:       telemetry,
		egressLauncher:  egressLauncher,
		serverInfo:      serverInfo,
		participants:    make(map[livekit.ParticipantIdentity]types.LocalParticipant),
		participantOpts: make(map[livekit.ParticipantIdentity]*ParticipantOptions),
		bufferFactory:   buffer.NewBufferFactory(config.Receiver.PacketBufferSize),
		batchedUpdates:  make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo),
		closed:          make(chan struct{}),
	}
	if r.protoRoom.EmptyTimeout == 0 {
		r.protoRoom.EmptyTimeout = DefaultEmptyTimeout
	}
	if r.protoRoom.CreationTime == 0 {
		r.protoRoom.CreationTime = time.Now().Unix()
	}

	go r.audioUpdateWorker()
	go r.connectionQualityWorker()
	go r.subscriberBroadcastWorker()

	return r
}

func (r *Room) ToProto() *livekit.Room {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return proto.Clone(r.protoRoom).(*livekit.Room)
}

func (r *Room) Name() livekit.RoomName {
	return livekit.RoomName(r.protoRoom.Name)
}

func (r *Room) ID() livekit.RoomID {
	return livekit.RoomID(r.protoRoom.Sid)
}

func (r *Room) GetParticipant(identity livekit.ParticipantIdentity) types.LocalParticipant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.participants[identity]
}

func (r *Room) GetParticipantBySid(participantID livekit.ParticipantID) types.LocalParticipant {
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
	participants := make([]types.LocalParticipant, 0, len(r.participants))
	for _, p := range r.participants {
		participants = append(participants, p)
	}
	return participants
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
	return r.bufferFactory
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

func (r *Room) Join(participant types.LocalParticipant, opts *ParticipantOptions, iceServers []*livekit.ICEServer) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.IsClosed() {
		prometheus.ServiceOperationCounter.WithLabelValues("participant_join", "error", "room_closed").Add(1)
		return ErrRoomClosed
	}

	if r.participants[participant.Identity()] != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("participant_join", "error", "already_joined").Add(1)
		return ErrAlreadyJoined
	}

	if r.protoRoom.MaxParticipants > 0 && len(r.participants) >= int(r.protoRoom.MaxParticipants) {
		prometheus.ServiceOperationCounter.WithLabelValues("participant_join", "error", "max_exceeded").Add(1)
		return ErrMaxParticipantsExceeded
	}

	if r.FirstJoinedAt() == 0 {
		r.joinedAt.Store(time.Now().Unix())
	}
	if !participant.Hidden() {
		r.protoRoom.NumParticipants++
	}

	// it's important to set this before connection, we don't want to miss out on any publishedTracks
	participant.OnTrackPublished(r.onTrackPublished)
	participant.OnStateChange(func(p types.LocalParticipant, oldState livekit.ParticipantInfo_State) {
		r.Logger.Debugw("participant state changed",
			"state", p.State(),
			"participant", p.Identity(),
			"pID", p.ID(),
			"oldState", oldState)
		if r.onParticipantChanged != nil {
			r.onParticipantChanged(participant)
		}
		r.broadcastParticipantState(p, broadcastOptions{skipSource: true})

		state := p.State()
		if state == livekit.ParticipantInfo_ACTIVE {
			// subscribe participant to existing publishedTracks
			r.subscribeToExistingTracks(p)

			// start the workers once connectivity is established
			p.Start()

			r.telemetry.ParticipantActive(context.Background(), r.ToProto(), p.ToProto(), &livekit.AnalyticsClientMeta{
				ClientConnectTime: uint32(time.Since(p.ConnectedAt()).Milliseconds()),
				ConnectionType:    string(p.GetICEConnectionType()),
			})
		} else if state == livekit.ParticipantInfo_DISCONNECTED {
			// remove participant from room
			go r.RemoveParticipant(p.Identity(), types.ParticipantCloseReasonStateDisconnected)
		}
	})
	participant.OnTrackUpdated(r.onTrackUpdated)
	participant.OnParticipantUpdate(r.onParticipantUpdate)
	participant.OnDataPacket(r.onDataPacket)
	participant.OnSubscribedTo(func(p types.LocalParticipant, publisherID livekit.ParticipantID) {
		go func() {
			// when a participant subscribes to another participant,
			// send speaker update if the subscribed to participant is active.
			speakers := r.GetActiveSpeakers()
			for _, speaker := range speakers {
				if livekit.ParticipantID(speaker.Sid) == publisherID {
					_ = p.SendSpeakerUpdate(speakers)
					break
				}
			}

			// send connection quality of subscribed to participant
			pub := r.GetParticipantBySid(publisherID)
			if pub != nil && pub.State() == livekit.ParticipantInfo_ACTIVE {
				update := &livekit.ConnectionQualityUpdate{}
				update.Updates = append(update.Updates, pub.GetConnectionQuality())
				_ = p.SendConnectionQualityUpdate(update)
			}
		}()
	})
	r.Logger.Infow("new participant joined",
		"pID", participant.ID(),
		"participant", participant.Identity(),
		"protocol", participant.ProtocolVersion(),
		"options", opts)

	if participant.IsRecorder() && !r.protoRoom.ActiveRecording {
		r.protoRoom.ActiveRecording = true
		r.sendRoomUpdateLocked()
	}

	r.participants[participant.Identity()] = participant
	r.participantOpts[participant.Identity()] = opts

	if r.onParticipantChanged != nil {
		r.onParticipantChanged(participant)
	}

	time.AfterFunc(time.Minute, func() {
		state := participant.State()
		if state == livekit.ParticipantInfo_JOINING || state == livekit.ParticipantInfo_JOINED {
			r.RemoveParticipant(participant.Identity(), types.ParticipantCloseReasonJoinTimeout)
		}
	})

	joinResponse := r.createJoinResponseLocked(participant, iceServers)
	if err := participant.SendJoinResponse(joinResponse); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("participant_join", "error", "send_response").Add(1)
		return err
	}

	participant.SetMigrateState(types.MigrateStateComplete)

	if participant.SubscriberAsPrimary() {
		// initiates sub connection as primary
		if participant.ProtocolVersion().SupportFastStart() {
			go func() {
				r.subscribeToExistingTracks(participant)
				participant.Negotiate(true)
			}()
		} else {
			participant.Negotiate(true)
		}
	}

	prometheus.ServiceOperationCounter.WithLabelValues("participant_join", "success", "").Add(1)

	return nil
}

func (r *Room) ResumeParticipant(p types.LocalParticipant, responseSink routing.MessageSink) error {
	// close previous sink, and link to new one
	p.CloseSignalConnection()
	p.SetResponseSink(responseSink)

	updates := ToProtoParticipants(r.GetParticipants())
	if err := p.SendParticipantUpdate(updates); err != nil {
		return err
	}

	p.ICERestart(nil)
	return nil
}

func (r *Room) RemoveParticipant(identity livekit.ParticipantIdentity, reason types.ParticipantCloseReason) {
	r.lock.Lock()
	p, ok := r.participants[identity]
	if ok {
		delete(r.participants, identity)
		delete(r.participantOpts, identity)
		if !p.Hidden() {
			r.protoRoom.NumParticipants--
		}
	}

	if (p != nil && p.IsRecorder()) || r.protoRoom.ActiveRecording {
		activeRecording := false
		for _, op := range r.participants {
			if op.IsRecorder() {
				activeRecording = true
				break
			}
		}

		if r.protoRoom.ActiveRecording != activeRecording {
			r.protoRoom.ActiveRecording = activeRecording
			r.sendRoomUpdateLocked()
		}
	}
	r.lock.Unlock()

	if !ok {
		return
	}

	// send broadcast only if it's not already closed
	sendUpdates := p.State() != livekit.ParticipantInfo_DISCONNECTED

	p.OnTrackUpdated(nil)
	p.OnTrackPublished(nil)
	p.OnStateChange(nil)
	p.OnParticipantUpdate(nil)
	p.OnDataPacket(nil)
	p.OnSubscribedTo(nil)

	// close participant as well
	r.Logger.Infow("closing participant for removal", "pID", p.ID(), "participant", p.Identity())
	_ = p.Close(true, reason)

	r.lock.RLock()
	if len(r.participants) == 0 {
		r.leftAt.Store(time.Now().Unix())
	}
	r.lock.RUnlock()

	if sendUpdates {
		if r.onParticipantChanged != nil {
			r.onParticipantChanged(p)
		}
		r.broadcastParticipantState(p, broadcastOptions{skipSource: true})
	}
}

func (r *Room) UpdateSubscriptions(
	participant types.LocalParticipant,
	trackIDs []livekit.TrackID,
	participantTracks []*livekit.ParticipantTracks,
	subscribe bool,
) error {
	// find all matching tracks
	trackPublishers := make(map[livekit.TrackID]types.Participant)
	participants := r.GetParticipants()
	for _, trackID := range trackIDs {
		for _, p := range participants {
			track := p.GetPublishedTrack(trackID)
			if track != nil {
				trackPublishers[trackID] = p
				break
			}
		}
	}

	for _, pt := range participantTracks {
		p := r.GetParticipantBySid(livekit.ParticipantID(pt.ParticipantSid))
		if p == nil {
			continue
		}
		for _, trackID := range livekit.StringsAsTrackIDs(pt.TrackSids) {
			trackPublishers[trackID] = p
		}
	}

	// handle subscription changes
	for trackID, publisher := range trackPublishers {
		if subscribe {
			if _, err := publisher.AddSubscriber(participant, types.AddSubscriberParams{TrackIDs: []livekit.TrackID{trackID}}); err != nil {
				return err
			}
		} else {
			publisher.RemoveSubscriber(participant, trackID, false)
		}
	}
	return nil
}

func (r *Room) SyncState(participant types.LocalParticipant, state *livekit.SyncState) error {
	return nil
}

func (r *Room) UpdateSubscriptionPermission(participant types.LocalParticipant, subscriptionPermission *livekit.SubscriptionPermission) error {
	return participant.UpdateSubscriptionPermission(subscriptionPermission, nil, r.GetParticipant, r.GetParticipantBySid)
}

func (r *Room) RemoveDisallowedSubscriptions(sub types.LocalParticipant, disallowedSubscriptions map[livekit.TrackID]livekit.ParticipantID) {
	for trackID, publisherID := range disallowedSubscriptions {
		pub := r.GetParticipantBySid(publisherID)
		if pub == nil {
			continue
		}

		pub.RemoveSubscriber(sub, trackID, false)
	}
}

func (r *Room) SetParticipantPermission(participant types.LocalParticipant, permission *livekit.ParticipantPermission) error {
	hadCanSubscribe := participant.CanSubscribe()
	participant.SetPermission(permission)
	// when subscribe perms are given, trigger autosub
	if !hadCanSubscribe && participant.CanSubscribe() {
		if participant.State() == livekit.ParticipantInfo_ACTIVE {
			if r.subscribeToExistingTracks(participant) == 0 {
				// start negotiating even if there are other media tracks to subscribe
				// we'll need to set the participant up to receive data
				participant.Negotiate(false)
			}
		}
	}

	return nil
}

func (r *Room) UpdateVideoLayers(participant types.Participant, updateVideoLayers *livekit.UpdateVideoLayers) error {
	return participant.UpdateVideoLayers(updateVideoLayers)
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
		if !p.IsRecorder() {
			r.lock.Unlock()
			return
		}
	}

	timeout := r.protoRoom.EmptyTimeout
	var elapsed int64
	if r.FirstJoinedAt() > 0 {
		// exit 20s after
		elapsed = time.Now().Unix() - r.LastLeftAt()
		if timeout > DefaultRoomDepartureGrace {
			timeout = DefaultRoomDepartureGrace
		}
	} else {
		elapsed = time.Now().Unix() - r.protoRoom.CreationTime
	}
	r.lock.Unlock()

	if elapsed >= int64(timeout) {
		r.Close()
	}
}

func (r *Room) Close() {
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
	r.Logger.Infow("closing room")
	for _, p := range r.GetParticipants() {
		_ = p.Close(true, types.ParticipantCloseReasonRoomClose)
	}
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

func (r *Room) SendDataPacket(up *livekit.UserPacket, kind livekit.DataPacket_Kind) {
	dp := &livekit.DataPacket{
		Kind: kind,
		Value: &livekit.DataPacket_User{
			User: up,
		},
	}
	r.onDataPacket(nil, dp)
}

func (r *Room) SetMetadata(metadata string) {
	r.lock.Lock()
	r.protoRoom.Metadata = metadata
	r.lock.Unlock()

	r.lock.RLock()
	r.sendRoomUpdateLocked()
	r.lock.RUnlock()

	if r.onMetadataUpdate != nil {
		r.onMetadataUpdate(metadata)
	}
}

func (r *Room) sendRoomUpdateLocked() {
	// Send update to participants
	for _, p := range r.participants {
		if !p.IsReady() {
			continue
		}

		err := p.SendRoomUpdate(r.protoRoom)
		if err != nil {
			r.Logger.Warnw("failed to send room update", err, "participant", p.Identity())
		}
	}
}

func (r *Room) OnMetadataUpdate(f func(metadata string)) {
	r.onMetadataUpdate = f
}

func (r *Room) SimulateScenario(participant types.LocalParticipant, simulateScenario *livekit.SimulateScenario) error {
	switch scenario := simulateScenario.Scenario.(type) {
	case *livekit.SimulateScenario_SpeakerUpdate:
		r.Logger.Infow("simulating speaker update", "participant", participant.Identity())
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
		r.Logger.Infow("simulating migration", "participant", participant.Identity())
		// drop participant without necessarily cleaning up
		if err := participant.Close(false, types.ParticipantCloseReasonSimulateMigration); err != nil {
			return err
		}
	case *livekit.SimulateScenario_NodeFailure:
		r.Logger.Infow("simulating node failure", "participant", participant.Identity())
		// drop participant without necessarily cleaning up
		if err := participant.Close(false, types.ParticipantCloseReasonSimulateNodeFailure); err != nil {
			return err
		}
	case *livekit.SimulateScenario_ServerLeave:
		r.Logger.Infow("simulating server leave", "participant", participant.Identity())
		if err := participant.Close(true, types.ParticipantCloseReasonSimulateServerLeave); err != nil {
			return err
		}

	case *livekit.SimulateScenario_SwitchCandidateProtocol:
		r.Logger.Infow("simulating switch candidate protocol", "participant", participant.Identity())
		participant.ICERestart(&types.IceConfig{
			PreferSub: types.PreferCandidateType(scenario.SwitchCandidateProtocol),
			PreferPub: types.PreferCandidateType(scenario.SwitchCandidateProtocol),
		})
	}
	return nil
}

// checks if participant should be autosubscribed to new tracks, assumes lock is already acquired
func (r *Room) autoSubscribe(participant types.LocalParticipant) bool {
	if !participant.CanSubscribe() {
		return false
	}

	opts := r.participantOpts[participant.Identity()]
	// default to true if no options are set
	if opts != nil && !opts.AutoSubscribe {
		return false
	}
	return true
}

func (r *Room) createJoinResponseLocked(participant types.LocalParticipant, iceServers []*livekit.ICEServer) *livekit.JoinResponse {
	// gather other participants and send join response
	otherParticipants := make([]*livekit.ParticipantInfo, 0, len(r.participants))
	for _, p := range r.participants {
		if p.ID() != participant.ID() && !p.Hidden() {
			otherParticipants = append(otherParticipants, p.ToProto())
		}
	}

	return &livekit.JoinResponse{
		Room:              r.protoRoom,
		Participant:       participant.ToProto(),
		OtherParticipants: otherParticipants,
		ServerVersion:     r.serverInfo.Version,
		ServerRegion:      r.serverInfo.Region,
		IceServers:        iceServers,
		// indicates both server and client support subscriber as primary
		SubscriberPrimary:   participant.SubscriberAsPrimary(),
		ClientConfiguration: participant.GetClientConfiguration(),
		// sane defaults for ping interval & timeout
		PingInterval: 10,
		PingTimeout:  20,
		ServerInfo:   r.serverInfo,
	}
}

// a ParticipantImpl in the room added a new remoteTrack, subscribe other participants to it
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

		r.Logger.Debugw("subscribing to new track",
			"participant", existingParticipant.Identity(),
			"pID", existingParticipant.ID(),
			"publisher", participant.Identity(),
			"publisherID", participant.ID(),
			"trackID", track.ID())
		if _, err := participant.AddSubscriber(existingParticipant, types.AddSubscriberParams{TrackIDs: []livekit.TrackID{track.ID()}}); err != nil {
			r.Logger.Errorw("could not subscribe to remoteTrack", err,
				"participant", existingParticipant.Identity(),
				"pID", existingParticipant.ID(),
				"publisher", participant.Identity(),
				"publisherID", participant.ID(),
				"trackID", track.ID())
		}
	}

	if r.onParticipantChanged != nil {
		r.onParticipantChanged(participant)
	}
	r.lock.RUnlock()

	// auto track egress
	if r.internal != nil && r.internal.TrackEgress != nil {
		if err := StartTrackEgress(
			context.Background(),
			r.egressLauncher,
			r.telemetry,
			r.internal.TrackEgress,
			track,
			r.Name(),
			r.ID(),
		); err != nil {
			r.Logger.Errorw("failed to launch track egress", err)
		}
	}
}

func (r *Room) onTrackUpdated(p types.LocalParticipant, _ types.MediaTrack) {
	// send track updates to everyone, especially if track was updated by admin
	r.broadcastParticipantState(p, broadcastOptions{})
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onParticipantUpdate(p types.LocalParticipant) {
	// immediately notify when permissions or metadata changed
	r.broadcastParticipantState(p, broadcastOptions{immediate: true})
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onDataPacket(source types.LocalParticipant, dp *livekit.DataPacket) {
	dest := dp.GetUser().GetDestinationSids()

	for _, op := range r.GetParticipants() {
		if op.State() != livekit.ParticipantInfo_ACTIVE {
			continue
		}
		if source != nil && op.ID() == source.ID() {
			continue
		}
		if len(dest) > 0 {
			found := false
			for _, dID := range dest {
				if op.ID() == livekit.ParticipantID(dID) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		err := op.SendDataPacket(dp)
		if err != nil {
			r.Logger.Infow("send data packet error", "error", err, "participant", op.Identity())
		}
	}
}

func (r *Room) subscribeToExistingTracks(p types.LocalParticipant) int {
	r.lock.RLock()
	shouldSubscribe := r.autoSubscribe(p)
	r.lock.RUnlock()
	if !shouldSubscribe {
		return 0
	}

	tracksAdded := 0
	for _, op := range r.GetParticipants() {
		if p.ID() == op.ID() {
			// don't send to itself
			continue
		}

		// subscribe to all
		n, err := op.AddSubscriber(p, types.AddSubscriberParams{AllTracks: true})
		if err != nil {
			// TODO: log error? or disconnect?
			r.Logger.Errorw("could not subscribe to publisher", err,
				"participant", p.Identity(),
				"pID", p.ID(),
				"publisher", op.Identity(),
				"publisherID", op.ID(),
			)
		}
		tracksAdded += n
	}
	if tracksAdded > 0 {
		r.Logger.Debugw("subscribed participants to existing tracks", "trackID", tracksAdded)
	}
	return tracksAdded
}

// broadcast an update about participant p
func (r *Room) broadcastParticipantState(p types.LocalParticipant, opts broadcastOptions) {
	pi := p.ToProto()

	if p.Hidden() {
		if !opts.skipSource {
			// send update only to hidden participant
			err := p.SendParticipantUpdate([]*livekit.ParticipantInfo{pi})
			if err != nil {
				r.Logger.Errorw("could not send update to participant", err,
					"participant", p.Identity(), "pID", p.ID())
			}
		}
		return
	}

	updates := r.pushAndDequeueUpdates(pi, opts.immediate)
	r.sendParticipantUpdates(updates)
}

func (r *Room) sendParticipantUpdates(updates []*livekit.ParticipantInfo) {
	for _, op := range r.GetParticipants() {
		// skip closed participants
		if op.State() == livekit.ParticipantInfo_DISCONNECTED {
			continue
		}

		err := op.SendParticipantUpdate(updates)
		if err != nil {
			r.Logger.Errorw("could not send update to participant", err,
				"participant", op.Identity(), "pID", op.ID())
		}
	}
}

// for protocol 2, send all active speakers
func (r *Room) sendActiveSpeakers(speakers []*livekit.SpeakerInfo) {
	dp := &livekit.DataPacket{
		Kind: livekit.DataPacket_LOSSY,
		Value: &livekit.DataPacket_Speaker{
			Speaker: &livekit.ActiveSpeakerUpdate{
				Speakers: speakers,
			},
		},
	}

	for _, p := range r.GetParticipants() {
		if p.ProtocolVersion().HandlesDataPackets() && !p.ProtocolVersion().SupportsSpeakerChanged() {
			_ = p.SendDataPacket(dp)
		}
	}
}

// for protocol 3, send only changed updates
func (r *Room) sendSpeakerChanges(speakers []*livekit.SpeakerInfo) {
	for _, p := range r.GetParticipants() {
		if p.ProtocolVersion().SupportsSpeakerChanged() {
			_ = p.SendSpeakerUpdate(speakers)
		}
	}
}

// push a participant update for batched broadcast, optionally returning immediate updates to broadcast.
// it handles the following scenarios
// * subscriber-only updates will be queued for batch updates
// * publisher & immediate updates will be returned without queuing
// * when the SID changes, it will return both updates, with the earlier participant set to disconnected
func (r *Room) pushAndDequeueUpdates(pi *livekit.ParticipantInfo, isImmediate bool) []*livekit.ParticipantInfo {
	r.batchedUpdatesMu.Lock()
	defer r.batchedUpdatesMu.Unlock()

	var updates []*livekit.ParticipantInfo
	identity := livekit.ParticipantIdentity(pi.Identity)
	existing := r.batchedUpdates[identity]
	shouldSend := isImmediate || pi.IsPublisher

	if existing != nil {
		if pi.Sid != existing.Sid {
			// session change, need to send immediately
			isImmediate = true
			existing.State = livekit.ParticipantInfo_DISCONNECTED
			updates = append(updates, existing)
		} else if pi.Version < existing.Version {
			// out of order update
			return nil
		} else if shouldSend {
			updates = append(updates, existing)
		}
	}

	if shouldSend {
		// include any queued update, and return
		delete(r.batchedUpdates, identity)
		updates = append(updates, pi)
	} else {
		// enqueue for batch
		r.batchedUpdates[identity] = pi
	}

	return updates
}

func (r *Room) subscriberBroadcastWorker() {
	ticker := time.NewTicker(subscriberUpdateInterval)
	defer ticker.Stop()

	for !r.IsClosed() {
		select {
		case <-r.closed:
			return
		case <-ticker.C:
			r.batchedUpdatesMu.Lock()
			updatesMap := r.batchedUpdates
			r.batchedUpdates = make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo)
			r.batchedUpdatesMu.Unlock()

			if len(updatesMap) == 0 {
				continue
			}

			updates := make([]*livekit.ParticipantInfo, 0, len(updatesMap))
			for _, pi := range updatesMap {
				updates = append(updates, pi)
			}
			r.sendParticipantUpdates(updates)
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
				inactiveSpeaker := proto.Clone(speaker).(*livekit.SpeakerInfo)
				inactiveSpeaker.Level = 0
				inactiveSpeaker.Active = false
				changedSpeakers = append(changedSpeakers, inactiveSpeaker)
			}
		}

		// see if an update is needed
		if len(changedSpeakers) > 0 {
			r.sendActiveSpeakers(activeSpeakers)
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
				r.Logger.Warnw("could not send connection quality update", err,
					"participant", op.Identity())
			}
		}

		prevConnectionInfos = nowConnectionInfos
	}
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
