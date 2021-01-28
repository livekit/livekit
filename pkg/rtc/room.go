package rtc

import (
	"sync"
	"time"

	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/proto/livekit"
)

type Room struct {
	livekit.Room
	config WebRTCConfig
	lock   sync.RWMutex
	// map of participantId -> Participant
	participants map[string]types.Participant
	hasJoined    bool
	isClosed     bool
	onClose      func()
}

func NewRoom(room *livekit.Room, config WebRTCConfig) *Room {
	return &Room{
		Room:         *room,
		config:       config,
		lock:         sync.RWMutex{},
		participants: make(map[string]types.Participant),
	}
}

func (r *Room) GetParticipant(id string) types.Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.participants[id]
}

func (r *Room) GetParticipants() []types.Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return funk.Values(r.participants).([]types.Participant)
}

func (r *Room) Join(participant types.Participant) error {
	if r.isClosed {
		return ErrRoomClosed
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	if r.MaxParticipants > 0 && int(r.MaxParticipants) == len(r.participants) {
		return ErrMaxParticipantsExceeded
	}

	r.hasJoined = true

	// it's important to set this before connection, we don't want to miss out on any publishedTracks
	participant.OnTrackPublished(r.onTrackAdded)
	participant.OnStateChange(func(p types.Participant, oldState livekit.ParticipantInfo_State) {
		logger.Debugw("participant state changed", "state", p.State(), "participant", p.ID(),
			"oldState", oldState)
		r.broadcastParticipantState(p)

		if oldState == livekit.ParticipantInfo_JOINING && p.State() == livekit.ParticipantInfo_JOINED {
			// subscribe participant to existing publishedTracks
			for _, op := range r.participants {
				if p.ID() == op.ID() {
					// don't send to itself
					continue
				}
				if err := op.AddSubscriber(p); err != nil {
					// TODO: log error? or disconnect?
					logger.Errorw("could not subscribe to participant",
						"dstParticipant", p.ID(),
						"srcParticipant", op.ID())
				}
			}
			// start the workers once connectivity is established
			p.Start()
		} else if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			// remove participant from room
			go r.RemoveParticipant(p.ID())
		}
	})
	participant.OnTrackUpdated(r.onTrackUpdated)

	logger.Infow("new participant joined",
		"id", participant.ID(),
		"name", participant.Name(),
		"roomId", r.Sid)

	r.participants[participant.ID()] = participant

	// gather other participants and send join response
	otherParticipants := make([]types.Participant, 0, len(r.participants))
	for _, p := range r.participants {
		if p.ID() != participant.ID() {
			otherParticipants = append(otherParticipants, p)
		}
	}

	return participant.SendJoinResponse(&r.Room, otherParticipants)
}

func (r *Room) RemoveParticipant(id string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if p, ok := r.participants[id]; ok {
		// avoid blocking lock
		go func() {
			Recover()
			// also stop connection if needed
			p.Close()
		}()
	}

	delete(r.participants, id)

	go r.CloseIfEmpty()
}

// Close the room if all participants had left, or it's still empty past timeout
func (r *Room) CloseIfEmpty() {
	if r.isClosed {
		return
	}

	r.lock.RLock()
	numParticipants := len(r.participants)
	r.lock.RUnlock()

	if numParticipants > 0 {
		return
	}

	elapsed := uint32(time.Now().Unix() - r.CreationTime)
	logger.Infow("comparing elapsed", "elapsed", elapsed, "timeout", r.EmptyTimeout)
	if r.hasJoined || (r.EmptyTimeout > 0 && elapsed >= r.EmptyTimeout) {
		r.isClosed = true
		if r.onClose != nil {
			r.onClose()
		}
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
			"srcParticipant", participant.ID(),
			"remoteTrack", track.ID(),
			"dstParticipant", existingParticipant.ID())
		if err := track.AddSubscriber(existingParticipant); err != nil {
			logger.Errorw("could not subscribe to remoteTrack",
				"srcParticipant", participant.ID(),
				"remoteTrack", track.ID(),
				"dstParticipant", existingParticipant.ID())
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
				"participant", p.ID(),
				"err", err)
		}
	}
}
