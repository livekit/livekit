package rtc

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/zapr"
	"github.com/livekit/protocol/utils"
	"github.com/pion/ion-sfu/pkg/buffer"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	livekit "github.com/livekit/livekit-server/proto"
)

const (
	DefaultEmptyTimeout       = 5 * 60 // 5m
	DefaultRoomDepartureGrace = 20
	AudioLevelQuantization    = 8 // ideally power of 2 to minimize float decimal
)

type Room struct {
	Room       *livekit.Room
	config     WebRTCConfig
	iceServers []*livekit.ICEServer
	lock       sync.RWMutex
	// map of identity -> Participant
	participants    map[string]types.Participant
	participantOpts map[string]*ParticipantOptions
	bufferFactory   *buffer.Factory

	// time the first participant joined the room
	joinedAt atomic.Value
	// time that the last participant left the room
	leftAt   atomic.Value
	isClosed utils.AtomicFlag

	// for active speaker updates
	audioConfig *config.AudioConfig

	statsReporter *RoomStatsReporter

	onParticipantChanged func(p types.Participant)
	onClose              func()
}

type ParticipantOptions struct {
	AutoSubscribe bool
}

func NewRoom(room *livekit.Room, config WebRTCConfig, iceServers []*livekit.ICEServer, audioConfig *config.AudioConfig) *Room {
	r := &Room{
		Room:            proto.Clone(room).(*livekit.Room),
		config:          config,
		iceServers:      iceServers,
		audioConfig:     audioConfig,
		statsReporter:   NewRoomStatsReporter(room.Name),
		participants:    make(map[string]types.Participant),
		participantOpts: make(map[string]*ParticipantOptions),
		bufferFactory:   buffer.NewBufferFactory(config.Receiver.packetBufferSize, zapr.NewLogger(logger.GetLogger())),
	}
	if r.Room.EmptyTimeout == 0 {
		r.Room.EmptyTimeout = DefaultEmptyTimeout
	}
	if r.Room.CreationTime == 0 {
		r.Room.CreationTime = time.Now().Unix()
	}
	r.statsReporter.RoomStarted()
	go r.audioUpdateWorker()
	return r
}

func (r *Room) GetParticipant(identity string) types.Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.participants[identity]
}

func (r *Room) GetParticipants() []types.Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	participants := make([]types.Participant, 0, len(r.participants))
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
			Sid:    p.ID(),
			Level:  ConvertAudioLevel(level),
			Active: active,
		})
	}

	sort.Slice(speakers, func(i, j int) bool {
		return speakers[i].Level > speakers[j].Level
	})
	return speakers
}

func (r *Room) GetStatsReporter() *RoomStatsReporter {
	return r.statsReporter
}

func (r *Room) GetBufferFactor() *buffer.Factory {
	return r.bufferFactory
}

func (r *Room) FirstJoinedAt() int64 {
	j := r.joinedAt.Load()
	if t, ok := j.(int64); ok {
		return t
	}
	return 0
}

func (r *Room) LastLeftAt() int64 {
	l := r.leftAt.Load()
	if t, ok := l.(int64); ok {
		return t
	}
	return 0
}

func (r *Room) Join(participant types.Participant, opts *ParticipantOptions) error {
	if r.isClosed.Get() {
		return ErrRoomClosed
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	if r.participants[participant.Identity()] != nil {
		return ErrAlreadyJoined
	}

	if r.Room.MaxParticipants > 0 && int(r.Room.MaxParticipants) == len(r.participants) {
		return ErrMaxParticipantsExceeded
	}

	if r.FirstJoinedAt() == 0 {
		r.joinedAt.Store(time.Now().Unix())
	}

	r.statsReporter.AddParticipant()

	// it's important to set this before connection, we don't want to miss out on any publishedTracks
	participant.OnTrackPublished(r.onTrackPublished)
	participant.OnStateChange(func(p types.Participant, oldState livekit.ParticipantInfo_State) {
		logger.Debugw("participant state changed", "state", p.State(), "participant", p.Identity(),
			"oldState", oldState)
		if r.onParticipantChanged != nil {
			r.onParticipantChanged(participant)
		}
		r.broadcastParticipantState(p, true)

		state := p.State()
		if state == livekit.ParticipantInfo_ACTIVE {
			if p.UpdateAfterActive() {
				_ = p.SendParticipantUpdate(ToProtoParticipants(r.GetParticipants()))
			}

			// subscribe participant to existing publishedTracks
			r.subscribeToExistingTracks(p)

			// start the workers once connectivity is established
			p.Start()

		} else if state == livekit.ParticipantInfo_DISCONNECTED {
			// remove participant from room
			go r.RemoveParticipant(p.Identity())
		}
	})
	participant.OnTrackUpdated(r.onTrackUpdated)
	participant.OnMetadataUpdate(r.onParticipantMetadataUpdate)
	participant.OnDataPacket(r.onDataPacket)
	logger.Infow("new participant joined",
		"id", participant.ID(),
		"participant", participant.Identity(),
		"roomId", r.Room.Sid)

	r.participants[participant.Identity()] = participant
	r.participantOpts[participant.Identity()] = opts

	// gather other participants and send join response
	otherParticipants := make([]types.Participant, 0, len(r.participants))
	for _, p := range r.participants {
		if p.ID() != participant.ID() {
			otherParticipants = append(otherParticipants, p)
		}
	}

	if r.onParticipantChanged != nil {
		r.onParticipantChanged(participant)
	}

	time.AfterFunc(time.Minute, func() {
		state := participant.State()
		if state == livekit.ParticipantInfo_JOINING || state == livekit.ParticipantInfo_JOINED {
			r.RemoveParticipant(participant.Identity())
		}
	})

	return participant.SendJoinResponse(r.Room, otherParticipants, r.iceServers)
}

func (r *Room) RemoveParticipant(identity string) {
	r.lock.Lock()
	p, ok := r.participants[identity]
	if ok {
		delete(r.participants, identity)
		delete(r.participantOpts, identity)
	}
	r.lock.Unlock()
	if !ok {
		return
	}
	r.statsReporter.SubParticipant()

	// send broadcast only if it's not already closed
	sendUpdates := p.State() != livekit.ParticipantInfo_DISCONNECTED

	p.OnTrackUpdated(nil)
	p.OnTrackPublished(nil)
	p.OnStateChange(nil)
	p.OnMetadataUpdate(nil)
	p.OnDataPacket(nil)

	// close participant as well
	_ = p.Close()

	r.lock.RLock()
	if len(r.participants) == 0 {
		r.leftAt.Store(time.Now().Unix())
	}
	r.lock.RUnlock()

	if sendUpdates {
		if r.onParticipantChanged != nil {
			r.onParticipantChanged(p)
		}
		r.broadcastParticipantState(p, true)
	}
}

func (r *Room) UpdateSubscriptions(participant types.Participant, trackIds []string, subscribe bool) error {
	if !participant.CanSubscribe() {
		return ErrCannotSubscribe
	}

	// find all matching tracks
	var tracks []types.PublishedTrack
	participants := r.GetParticipants()
	for _, p := range participants {
		for _, sid := range trackIds {
			for _, track := range p.GetPublishedTracks() {
				if sid == track.ID() {
					tracks = append(tracks, track)
				}
			}
		}
	}

	// handle subscription changes
	for _, track := range tracks {
		if subscribe {
			if err := track.AddSubscriber(participant); err != nil {
				return err
			}
		} else {
			track.RemoveSubscriber(participant.ID())
		}
	}
	return nil
}

// CloseIfEmpty closes the room if all participants had left, or it's still empty past timeout
func (r *Room) CloseIfEmpty() {
	if r.isClosed.Get() {
		return
	}

	r.lock.RLock()
	numParticipants := len(r.participants)
	r.lock.RUnlock()

	if numParticipants > 0 {
		return
	}

	timeout := r.Room.EmptyTimeout
	var elapsed int64
	if r.FirstJoinedAt() > 0 {
		// exit 20s after
		elapsed = time.Now().Unix() - r.LastLeftAt()
		if timeout > DefaultRoomDepartureGrace {
			timeout = DefaultRoomDepartureGrace
		}
	} else {
		elapsed = time.Now().Unix() - r.Room.CreationTime
	}

	if elapsed >= int64(timeout) {
		r.Close()
	}
}

func (r *Room) Close() {
	if !r.isClosed.TrySet(true) {
		return
	}
	logger.Infow("closing room", "room", r.Room.Sid, "name", r.Room.Name)

	r.statsReporter.RoomEnded()
	if r.onClose != nil {
		r.onClose()
	}
}

func (r *Room) GetIncomingStats() PacketStats {
	return *r.statsReporter.incoming
}

func (r *Room) GetOutgoingStats() PacketStats {
	return *r.statsReporter.outgoing
}

func (r *Room) OnClose(f func()) {
	r.onClose = f
}

func (r *Room) OnParticipantChanged(f func(participant types.Participant)) {
	r.onParticipantChanged = f
}

// checks if participant should be autosubscribed to new tracks, assumes lock is already acquired
func (r *Room) autoSubscribe(participant types.Participant) bool {
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

// a ParticipantImpl in the room added a new remoteTrack, subscribe other participants to it
func (r *Room) onTrackPublished(participant types.Participant, track types.PublishedTrack) {
	// publish participant update, since track state is changed
	r.broadcastParticipantState(participant, true)

	r.lock.RLock()
	defer r.lock.RUnlock()

	// subscribe all existing participants to this PublishedTrack
	// this is the default behavior. in the future this could be more selective
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

		logger.Debugw("subscribing to new track",
			"source", participant.Identity(),
			"remoteTrack", track.ID(),
			"dest", existingParticipant.Identity())
		if err := track.AddSubscriber(existingParticipant); err != nil {
			logger.Errorw("could not subscribe to remoteTrack", err,
				"source", participant.Identity(),
				"remoteTrack", track.ID(),
				"dest", existingParticipant.Identity())
		}
	}

	if r.onParticipantChanged != nil {
		r.onParticipantChanged(participant)
	}
}

func (r *Room) onTrackUpdated(p types.Participant, _ types.PublishedTrack) {
	// send track updates to everyone, especially if track was updated by admin
	r.broadcastParticipantState(p, false)
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onParticipantMetadataUpdate(p types.Participant) {
	r.broadcastParticipantState(p, false)
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onDataPacket(source types.Participant, dp *livekit.DataPacket) {
	dest := dp.GetUser().GetDestinationSids()

	for _, op := range r.GetParticipants() {
		if op.State() != livekit.ParticipantInfo_ACTIVE {
			continue
		}
		if op.ID() == source.ID() {
			continue
		}
		if len(dest) > 0 {
			found := false
			for _, dSid := range dest {
				if op.ID() == dSid {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		_ = op.SendDataPacket(dp)
	}
}

func (r *Room) subscribeToExistingTracks(p types.Participant) {
	r.lock.RLock()
	shouldSubscribe := r.autoSubscribe(p)
	r.lock.RUnlock()
	if !shouldSubscribe {
		return
	}

	tracksAdded := 0
	for _, op := range r.GetParticipants() {
		if p.ID() == op.ID() {
			// don't send to itself
			continue
		}
		if n, err := op.AddSubscriber(p); err != nil {
			// TODO: log error? or disconnect?
			logger.Errorw("could not subscribe to participant", err,
				"dest", p.Identity(),
				"source", op.Identity())
		} else {
			tracksAdded += n
		}
	}
	if tracksAdded > 0 {
		logger.Debugw("subscribed participants to existing tracks", "tracks", tracksAdded)
	}
}

// broadcast an update about participant p
func (r *Room) broadcastParticipantState(p types.Participant, skipSource bool) {
	updates := ToProtoParticipants([]types.Participant{p})
	participants := r.GetParticipants()
	for _, op := range participants {
		// skip itself && closed participants
		if (skipSource && p.ID() == op.ID()) || op.State() == livekit.ParticipantInfo_DISCONNECTED {
			continue
		}

		err := op.SendParticipantUpdate(updates)
		if err != nil {
			logger.Errorw("could not send update to participant", err,
				"participant", p.Identity())
		}
	}
}

func (r *Room) sendSpeakerUpdates(speakers []*livekit.SpeakerInfo) {
	dp := &livekit.DataPacket{
		Kind: livekit.DataPacket_LOSSY,
		Value: &livekit.DataPacket_Speaker{
			Speaker: &livekit.ActiveSpeakerUpdate{
				Speakers: speakers,
			},
		},
	}

	for _, p := range r.GetParticipants() {
		if p.ProtocolVersion().HandlesDataPackets() {
			_ = p.SendDataPacket(dp)
		} else {
			_ = p.SendActiveSpeakers(speakers)
		}
	}
}

func (r *Room) audioUpdateWorker() {
	var smoothValues map[string]float32
	var smoothFactor float32
	var activeThreshold float32
	if ss := r.audioConfig.SmoothIntervals; ss > 1 {
		smoothValues = make(map[string]float32)
		// exponential moving average (EMA), same center of mass with simple moving average (SMA)
		smoothFactor = 2 / float32(ss+1)
		activeThreshold = ConvertAudioLevel(r.audioConfig.ActiveLevel)
	}

	var lastActiveSpeakers []*livekit.SpeakerInfo
	for {
		if r.isClosed.Get() {
			return
		}

		speakers := r.GetActiveSpeakers()
		if smoothValues != nil {
			for _, speaker := range speakers {
				sid := speaker.Sid
				level := smoothValues[sid]
				delete(smoothValues, sid)
				// exponential moving average (EMA)
				level += (speaker.Level - level) * smoothFactor
				speaker.Level = level
			}

			// ensure that previous active speakers are also included
			for sid, level := range smoothValues {
				delete(smoothValues, sid)
				level += -level * smoothFactor
				if level > activeThreshold {
					speakers = append(speakers, &livekit.SpeakerInfo{
						Sid:    sid,
						Level:  level,
						Active: true,
					})
				}
			}

			// smoothValues map is drained, now repopulate it back
			for _, speaker := range speakers {
				smoothValues[speaker.Sid] = speaker.Level
			}

			sort.Slice(speakers, func(i, j int) bool {
				return speakers[i].Level > speakers[j].Level
			})
		}

		const invAudioLevelQuantization = 1.0 / AudioLevelQuantization
		for _, speaker := range speakers {
			speaker.Level = float32(math.Ceil(float64(speaker.Level*AudioLevelQuantization)) * invAudioLevelQuantization)
		}

		// see if an update is needed
		if len(speakers) == len(lastActiveSpeakers) {
			for i, speaker := range speakers {
				if speaker.Level != lastActiveSpeakers[i].Level || speaker.Sid != lastActiveSpeakers[i].Sid {
					r.sendSpeakerUpdates(speakers)
					break
				}
			}
		} else {
			r.sendSpeakerUpdates(speakers)
		}

		lastActiveSpeakers = speakers

		time.Sleep(time.Duration(r.audioConfig.UpdateInterval) * time.Millisecond)
	}
}
