package service

import (
	"io"
	"sync"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/proto/livekit"
)

// RTC runner manages the lifecycles of a WebRTC connection
// it creates a new goroutine for each participant it manages.

type RTCRunner struct {
	lock         sync.RWMutex
	roomProvider RoomStore
	currentNode  routing.LocalNode
	router       routing.Router
	config       *rtc.WebRTCConfig
	rooms        map[string]*rtc.Room
}

func NewRTCRunner(rp RoomStore, router routing.Router, currentNode routing.LocalNode, config *rtc.WebRTCConfig) *RTCRunner {
	return &RTCRunner{
		lock:         sync.RWMutex{},
		roomProvider: rp,
		config:       config,
		router:       router,
		currentNode:  currentNode,
		rooms:        make(map[string]*rtc.Room),
	}
}

// starts WebRTC session when a new participant is connected
func (r *RTCRunner) StartSession(roomName, participantId, participantName string, requestSource routing.MessageSource, responseSink routing.MessageSink) {
	room, err := r.getOrCreateRoom(roomName)
	if err != nil {
		logger.Errorw("could not create room", "error", err)
		return
	}

	logger.Debugw("starting RTC session",
		"room", roomName,
		"participant", participantName,
		"num_participants", len(room.GetParticipants()),
	)

	pc, err := rtc.NewPeerConnection(r.config)
	if err != nil {
		logger.Errorw("could not create peerConnection", "error", err)
		return
	}

	participant, err := rtc.NewParticipant(participantId, participantName, pc, responseSink, r.config.Receiver)
	if err != nil {
		logger.Errorw("could not create participant", "error", err)
		return
	}

	// register participant to be on this server
	if err = r.router.SetParticipantRTCNode(participantId, r.currentNode.Id); err != nil {
		logger.Errorw("could not set RTC node", "error", err)
		return
	}

	// join room
	if err := room.Join(participant); err != nil {
		logger.Errorw("could not join room", "error", err)
		return
	}

	go r.sessionWorker(room, participant, requestSource)
}

func (r *RTCRunner) getOrCreateRoom(roomName string) (*rtc.Room, error) {
	r.lock.RLock()
	room := r.rooms[roomName]
	r.lock.RUnlock()

	if room != nil {
		return room, nil
	}

	// create new room, get details first
	ri, err := r.roomProvider.GetRoom(roomName)
	if err != nil {
		return nil, err
	}

	room = rtc.NewRoom(ri, *r.config)
	r.lock.Lock()
	r.rooms[roomName] = room
	r.lock.Unlock()

	return room, nil
}

func (r *RTCRunner) sessionWorker(room *rtc.Room, participant types.Participant, requestSource routing.MessageSource) {
	defer func() {
		logger.Debugw("RTC session finishing",
			"participant", participant.Name(),
			"room", room.Name,
		)
		// remove peer from room when participant leaves room
		room.RemoveParticipant(participant.ID())
	}()
	defer rtc.Recover()

	for {
		obj, err := requestSource.ReadMessage()
		if err == io.EOF {
			return
		}

		req := obj.(*livekit.SignalRequest)

		switch msg := req.Message.(type) {
		case *livekit.SignalRequest_Offer:
			_, err := participant.Answer(rtc.FromProtoSessionDescription(msg.Offer))
			if err != nil {
				logger.Errorw("could not handle join", "err", err, "participant", participant.ID())
				return
			}
		case *livekit.SignalRequest_AddTrack:
			logger.Debugw("publishing track", "participant", participant.ID(),
				"track", msg.AddTrack.Cid)
			participant.AddTrack(msg.AddTrack.Cid, msg.AddTrack.Name, msg.AddTrack.Type)
		case *livekit.SignalRequest_Answer:
			if participant.State() == livekit.ParticipantInfo_JOINING {
				logger.Errorw("cannot negotiate before peer offer", "participant", participant.ID())
				//conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot negotiate before peer offer"))
				return
			}
			sd := rtc.FromProtoSessionDescription(msg.Answer)
			err = participant.HandleAnswer(sd)
			if err != nil {
				logger.Errorw("could not handle answer", "participant", participant.ID(), "err", err)
				//conn.WriteJSON(
				//	jsonError(http.StatusInternalServerError, "could not handle negotiate", err.Error()))
				return
			}
		case *livekit.SignalRequest_Negotiate:
			participant.HandleClientNegotiation()
		case *livekit.SignalRequest_Trickle:
			if participant.State() == livekit.ParticipantInfo_JOINING {
				logger.Errorw("cannot trickle before peer offer", "participant", participant.ID())
				//conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot trickle before peer offer"))
				return
			}

			candidateInit := rtc.FromProtoTrickle(msg.Trickle)
			//logger.Debugw("adding peer candidate", "participant", participant.ID())
			if err := participant.AddICECandidate(candidateInit); err != nil {
				logger.Errorw("could not handle trickle", "participant", participant.ID(), "err", err)
				//conn.WriteJSON(
				//	jsonError(http.StatusInternalServerError, "could not handle trickle", err.Error()))
				return
			}
		case *livekit.SignalRequest_Mute:
			participant.SetTrackMuted(msg.Mute.Sid, msg.Mute.Muted)
		}
	}
}
