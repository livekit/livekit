package rtc

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

type Room struct {
	livekit.Room
	config WebRTCConfig
	lock   sync.RWMutex
	// map of participantId -> Participant
	participants map[string]*Participant
	// Client ID => list of tracks they are publishing
	//tracks map[string][]Track
}

func NewRoomForRequest(req *livekit.CreateRoomRequest, config *WebRTCConfig) (*Room, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}

	return &Room{
		Room: livekit.Room{
			Id:              utils.NewGuid(utils.RoomPrefix),
			EmptyTimeout:    req.EmptyTimeout,
			MaxParticipants: req.MaxParticipants,
			CreationTime:    time.Now().Unix(),
			Token:           id.String(),
		},
		config:       *config,
		lock:         sync.RWMutex{},
		participants: make(map[string]*Participant),
	}, nil
}

func (r *Room) GetParticipant(id string) *Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.participants[id]
}

func (r *Room) ToRoomInfo(node *livekit.Node) *livekit.RoomInfo {
	return &livekit.RoomInfo{
		Id:           r.Id,
		NodeIp:       node.Ip,
		CreationTime: r.CreationTime,
		Token:        r.Token,
	}
}

func (r *Room) Join(participant *Participant) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	log := logger.GetLogger()

	// it's important to set this before connection, we don't want to miss out on any tracks
	participant.OnPeerTrack = r.onTrackAdded
	participant.OnStateChange = func(p *Participant, oldState livekit.ParticipantInfo_State) {
		log.Debugw("participant state changed", "state", p.state, "participant", p.id)
		r.broadcastParticipantState(p)
	}

	log.Infow("new participant joined",
		"id", participant.ID(),
		"name", participant.Name(),
		"roomId", r.Id)

	// subscribe participant to existing tracks
	for _, p := range r.participants {
		if err := p.AddSubscriber(participant); err != nil {
			// TODO: log error? or disconnect?
			logger.GetLogger().Errorw("could not subscribe to participant",
				"dstParticipant", participant.ID(),
				"srcParticipant", p.ID())
		}
	}

	r.participants[participant.ID()] = participant

	// gather other participants and send join response
	otherParticipants := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		if p.id != participant.id {
			otherParticipants = append(otherParticipants, p)
		}
	}

	return participant.SendJoinResponse(otherParticipants)
}

func (r *Room) RemoveParticipant(id string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if p, ok := r.participants[id]; ok {
		// also stop connection if needed
		p.Close()
	}

	delete(r.participants, id)
}

// a peer in the room added a new mediaTrack, subscribe other participants to it
func (r *Room) onTrackAdded(peer *Participant, track *Track) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	// subscribe all existing participants to this mediaTrack
	for _, p := range r.participants {
		if p == peer {
			// skip publishing peer
			continue
		}
		if err := track.AddSubscriber(peer); err != nil {
			logger.GetLogger().Errorw("could not subscribe to mediaTrack",
				"srcParticipant", peer.ID(),
				"mediaTrack", track.id,
				"dstParticipant", p.ID())
		}
	}
}

func (r *Room) broadcastParticipantState(p *Participant) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	updates := ToProtoParticipants([]*Participant{p})
	for _, op := range r.participants {
		// skip itself
		if p.id == op.id {
			continue
		}

		err := op.SendParticipantUpdate(updates)
		if err != nil {
			logger.GetLogger().Errorw("could not send update to participant",
				"participant", p.id,
				"err", err)
		}
	}
}
