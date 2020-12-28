package rtc

import (
	"sync"
	"time"

	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

type Room struct {
	livekit.Room
	config WebRTCConfig
	lock   sync.RWMutex
	// map of participantId -> Participant
	participants map[string]Participant
	// Client ID => list of tracks they are publishing
	//tracks map[string][]Track
}

func NewRoomForRequest(req *livekit.CreateRoomRequest, config *WebRTCConfig) *Room {
	return &Room{
		Room: livekit.Room{
			Sid:             utils.NewGuid(utils.RoomPrefix),
			Name:            req.Name,
			EmptyTimeout:    req.EmptyTimeout,
			MaxParticipants: req.MaxParticipants,
			CreationTime:    time.Now().Unix(),
		},
		config:       *config,
		lock:         sync.RWMutex{},
		participants: make(map[string]Participant),
	}
}

func (r *Room) GetParticipant(id string) Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.participants[id]
}

func (r *Room) GetParticipants() []Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return funk.Values(r.participants).([]Participant)
}

func (r *Room) ToRoomInfo(node *livekit.Node) *livekit.RoomInfo {
	ri := &livekit.RoomInfo{
		Sid:          r.Sid,
		Name:         r.Name,
		CreationTime: r.CreationTime,
	}
	if node != nil {
		ri.NodeIp = node.Ip
	}
	return ri
}

func (r *Room) Join(participant Participant) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	log := logger.GetLogger()

	// it's important to set this before connection, we don't want to miss out on any tracks
	participant.OnTrackPublished(r.onTrackAdded)
	participant.OnStateChange(func(p Participant, oldState livekit.ParticipantInfo_State) {
		log.Debugw("participant state changed", "state", p.State(), "participant", p.ID())
		r.broadcastParticipantState(p)

		if oldState == livekit.ParticipantInfo_JOINING && p.State() == livekit.ParticipantInfo_JOINED {
			// subscribe participant to existing tracks
			for _, op := range r.participants {
				if p.ID() == op.ID() {
					// don't send to itself
					continue
				}
				if err := op.AddSubscriber(p); err != nil {
					// TODO: log error? or disconnect?
					logger.GetLogger().Errorw("could not subscribe to participant",
						"dstParticipant", p.ID(),
						"srcParticipant", op.ID())
				}
			}
			// start the workers once connectivity is established
			p.Start()
		}
	})

	log.Infow("new participant joined",
		"id", participant.ID(),
		"name", participant.Name(),
		"roomId", r.Sid)

	r.participants[participant.ID()] = participant

	// gather other participants and send join response
	otherParticipants := make([]Participant, 0, len(r.participants))
	for _, p := range r.participants {
		if p.ID() != participant.ID() {
			otherParticipants = append(otherParticipants, p)
		}
	}

	return participant.SendJoinResponse(r.ToRoomInfo(nil), otherParticipants)
}

func (r *Room) RemoveParticipant(id string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if p, ok := r.participants[id]; ok {
		// avoid blocking lock
		go func() {
			// also stop connection if needed
			p.Close()
			// update clients
			r.broadcastParticipantState(p)
		}()
	}

	delete(r.participants, id)
}

// a ParticipantImpl in the room added a new remoteTrack, subscribe other participants to it
func (r *Room) onTrackAdded(participant Participant, track PublishedTrack) {
	// publish participant update, since track state is changed
	r.broadcastParticipantState(participant)

	r.lock.RLock()
	defer r.lock.RUnlock()

	// subscribe all existing participants to this PublishedTrack
	// this is the default behavior. in the future this could be more selective
	for _, existingParticipant := range r.participants {
		if existingParticipant == participant {
			// skip publishing peer
			continue
		}
		if existingParticipant.State() != livekit.ParticipantInfo_JOINED {
			// not fully joined. don't subscribe yet
			continue
		}
		if err := track.AddSubscriber(existingParticipant); err != nil {
			logger.GetLogger().Errorw("could not subscribe to remoteTrack",
				"srcParticipant", participant.ID(),
				"remoteTrack", track.ID(),
				"dstParticipant", existingParticipant.ID())
		}
	}
}

func (r *Room) broadcastParticipantState(p Participant) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	updates := ToProtoParticipants([]Participant{p})
	for _, op := range r.participants {
		// skip itself
		if p.ID() == op.ID() {
			continue
		}

		err := op.SendParticipantUpdate(updates)
		if err != nil {
			logger.GetLogger().Errorw("could not send update to participant",
				"participant", p.ID(),
				"err", err)
		}
	}
}
