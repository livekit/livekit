package rtc

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	DefaultEmptyTimeout = 5 * 60 // 5 mins
)

type Room struct {
	livekit.Room
	config WebRTCConfig
	lock   sync.RWMutex
	// map of identity -> Participant
	participants map[string]types.Participant
	// time the first participant joined the room
	joinedAt atomic.Value
	// time that the last participant left the room
	leftAt   atomic.Value
	isClosed utils.AtomicFlag
	onClose  func()
}

func NewRoom(room *livekit.Room, config WebRTCConfig) *Room {
	r := &Room{
		Room:         *room,
		config:       config,
		lock:         sync.RWMutex{},
		participants: make(map[string]types.Participant),
	}
	if r.EmptyTimeout == 0 {
		r.EmptyTimeout = DefaultEmptyTimeout
	}
	if r.CreationTime == 0 {
		r.CreationTime = time.Now().Unix()
	}
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
	return funk.Values(r.participants).([]types.Participant)
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

func (r *Room) Join(participant types.Participant) error {
	if r.isClosed.Get() {
		return ErrRoomClosed
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	if r.participants[participant.Identity()] != nil {
		return ErrAlreadyJoined
	}

	if r.MaxParticipants > 0 && int(r.MaxParticipants) == len(r.participants) {
		return ErrMaxParticipantsExceeded
	}

	if r.FirstJoinedAt() == 0 {
		r.joinedAt.Store(time.Now().Unix())
	}

	// it's important to set this before connection, we don't want to miss out on any publishedTracks
	participant.OnTrackPublished(r.onTrackAdded)
	participant.OnStateChange(func(p types.Participant, oldState livekit.ParticipantInfo_State) {
		logger.Debugw("participant state changed", "state", p.State(), "participant", p.Identity(),
			"oldState", oldState)
		r.broadcastParticipantState(p)

		if oldState == livekit.ParticipantInfo_JOINING && p.State() == livekit.ParticipantInfo_JOINED {
			// subscribe participant to existing publishedTracks
			for _, op := range r.GetParticipants() {
				if p.ID() == op.ID() {
					// don't send to itself
					continue
				}
				if err := op.AddSubscriber(p); err != nil {
					// TODO: log error? or disconnect?
					logger.Errorw("could not subscribe to participant",
						"dest", p.Identity(),
						"source", op.Identity())
				}
			}
			// start the workers once connectivity is established
			p.Start()
		} else if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			// remove participant from room
			go r.RemoveParticipant(p.Identity())
		}
	})
	participant.OnTrackUpdated(r.onTrackUpdated)

	logger.Infow("new participant joined",
		"id", participant.ID(),
		"identity", participant.Identity(),
		"roomId", r.Sid)

	r.participants[participant.Identity()] = participant

	// gather other participants and send join response
	otherParticipants := make([]types.Participant, 0, len(r.participants))
	for _, p := range r.participants {
		if p.ID() != participant.ID() {
			otherParticipants = append(otherParticipants, p)
		}
	}

	return participant.SendJoinResponse(&r.Room, otherParticipants)
}

func (r *Room) RemoveParticipant(identity string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if p, ok := r.participants[identity]; ok {
		// avoid blocking lock
		go func() {
			Recover()
			// also stop connection if needed
			p.Close()
		}()
	}

	delete(r.participants, identity)

	if len(r.participants) == 0 {
		r.leftAt.Store(time.Now().Unix())
	}
}

// Close the room if all participants had left, or it's still empty past timeout
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

	var elapsed int64
	if r.FirstJoinedAt() > 0 {
		// compute elasped from last departure
		elapsed = time.Now().Unix() - r.LastLeftAt()
	} else {
		elapsed = time.Now().Unix() - r.CreationTime
	}
	if elapsed >= int64(r.EmptyTimeout) {
		r.Close()
	}
}

func (r *Room) Close() {
	logger.Infow("closing room", "room", r.Sid, "name", r.Name)
	if r.isClosed.TrySet(true) && r.onClose != nil {
		r.onClose()
	}
}

func (r *Room) OnClose(f func()) {
	r.onClose = f
}

// a ParticipantImpl in the room added a new remoteTrack, subscribe other participants to it
func (r *Room) onTrackAdded(participant types.Participant, track types.PublishedTrack) {
	// publish participant update, since track state is changed
	r.broadcastParticipantState(participant)

	r.lock.RLock()
	defer r.lock.RUnlock()

	// subscribe all existing participants to this PublishedTrack
	// this is the default behavior. in the future this could be more selective
	for _, existingParticipant := range r.participants {
		if existingParticipant == participant {
			// skip publishing participant
			continue
		}
		if !existingParticipant.IsReady() {
			// not fully joined. don't subscribe yet
			continue
		}
		logger.Debugw("subscribing to new track",
			"source", participant.Identity(),
			"remoteTrack", track.ID(),
			"dest", existingParticipant.Identity())
		if err := track.AddSubscriber(existingParticipant); err != nil {
			logger.Errorw("could not subscribe to remoteTrack",
				"source", participant.Identity(),
				"remoteTrack", track.ID(),
				"dest", existingParticipant.Identity())
		}
	}
}

func (r *Room) onTrackUpdated(p types.Participant, track types.PublishedTrack) {
	r.broadcastParticipantState(p)
}

// broadcast an update about participant p
func (r *Room) broadcastParticipantState(p types.Participant) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	updates := ToProtoParticipants([]types.Participant{p})
	for _, op := range r.participants {
		// skip itself && closed participants
		if p.ID() == op.ID() || op.State() == livekit.ParticipantInfo_DISCONNECTED {
			continue
		}

		err := op.SendParticipantUpdate(updates)
		if err != nil {
			logger.Errorw("could not send update to participant",
				"participant", p.Identity(),
				"err", err)
		}
	}
}
