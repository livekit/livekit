package service

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	roomPurgeSeconds = 24 * 60 * 60
)

// RoomManager manages rooms and its interaction with participants.
// It's responsible for creating, deleting rooms, as well as running sessions for participants
type RoomManager struct {
	lock        sync.RWMutex
	roomStore   RoomStore
	selector    routing.NodeSelector
	router      routing.Router
	currentNode routing.LocalNode
	config      *rtc.WebRTCConfig
	rooms       map[string]*rtc.Room
}

func NewRoomManager(rp RoomStore, router routing.Router, currentNode routing.LocalNode, selector routing.NodeSelector, config *rtc.WebRTCConfig) *RoomManager {
	return &RoomManager{
		lock:        sync.RWMutex{},
		roomStore:   rp,
		config:      config,
		router:      router,
		selector:    selector,
		currentNode: currentNode,
		rooms:       make(map[string]*rtc.Room),
	}
}

// CreateRoom creates a new room from a request and allocates it to a node to handle
// it'll also monitor its state, and cleans it up when appropriate
func (r *RoomManager) CreateRoom(req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	rm := &livekit.Room{
		Sid:             utils.NewGuid(utils.RoomPrefix),
		Name:            req.Name,
		EmptyTimeout:    req.EmptyTimeout,
		MaxParticipants: req.MaxParticipants,
		CreationTime:    time.Now().Unix(),
	}
	if err := r.roomStore.CreateRoom(rm); err != nil {
		return nil, err
	}

	nodeId := req.NodeId
	if nodeId == "" {
		// select a node for room
		nodes, err := r.router.ListNodes()
		if err != nil {
			return nil, err
		}

		node, err := r.selector.SelectNode(nodes, rm)
		if err != nil {
			return nil, err
		}
		nodeId = node.Id
	}

	logger.Debugw("selected node for room", "room", rm.Name, "node", nodeId)
	if err := r.router.SetNodeForRoom(req.Name, nodeId); err != nil {
		return nil, err
	}

	return rm, nil
}

// DeleteRoom completely deletes all room information, including active sessions, room store, and routing info
func (r *RoomManager) DeleteRoom(roomName string) error {
	logger.Infow("deleting room state", "room", roomName)
	r.lock.Lock()
	delete(r.rooms, roomName)
	r.lock.Unlock()

	var err, err2 error
	wg := sync.WaitGroup{}
	wg.Add(2)
	// clear routing information
	go func() {
		defer wg.Done()
		err = r.router.ClearRoomState(roomName)
	}()
	// also delete room from db
	go func() {
		defer wg.Done()
		err2 = r.roomStore.DeleteRoom(roomName)
	}()

	if err != nil {
		err = err2
	}

	return err
}

// clean up after old rooms that have been around for awhile
func (r *RoomManager) Cleanup() error {
	// cleanup rooms that have been left for over a day
	rooms, err := r.roomStore.ListRooms()
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	for _, room := range rooms {
		if (now - room.CreationTime) > roomPurgeSeconds {
			if err := r.DeleteRoom(room.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// starts WebRTC session when a new participant is connected, takes place on RTC node
func (r *RoomManager) StartSession(roomName, identity string, requestSource routing.MessageSource, responseSink routing.MessageSink) {
	room, err := r.getOrCreateRoom(roomName)
	if err != nil {
		logger.Errorw("could not create room", "error", err)
		return
	}

	// Use existing peer connection if it's already connected, perhaps from a different signal connection
	participant := room.GetParticipant(identity)
	if participant != nil {
		logger.Debugw("resuming RTC session",
			"room", roomName,
			"node", r.currentNode.Id,
			"participant", identity,
		)
		// close previous sink, and link to new one
		prevSink := participant.GetResponseSink()
		if prevSink != nil {
			prevSink.Close()
		}
		participant.SetResponseSink(responseSink)
		return
	}

	logger.Debugw("starting RTC session",
		"room", roomName,
		"node", r.currentNode.Id,
		"participant", identity,
		"num_participants", len(room.GetParticipants()),
	)

	pc, err := rtc.NewPeerConnection(r.config)
	if err != nil {
		logger.Errorw("could not create peerConnection", "error", err)
		return
	}

	participant, err = rtc.NewParticipant(identity, pc, responseSink, r.config.Receiver)
	if err != nil {
		logger.Errorw("could not create participant", "error", err)
		return
	}

	// join room
	if err := room.Join(participant); err != nil {
		logger.Errorw("could not join room", "error", err)
		return
	}

	go r.rtcSessionWorker(room, participant, requestSource)
}

// create the actual room object
func (r *RoomManager) getOrCreateRoom(roomName string) (*rtc.Room, error) {
	r.lock.RLock()
	room := r.rooms[roomName]
	r.lock.RUnlock()

	if room != nil {
		return room, nil
	}

	// create new room, get details first
	ri, err := r.roomStore.GetRoom(roomName)
	if err != nil {
		return nil, err
	}

	room = rtc.NewRoom(ri, *r.config)
	room.OnClose(func() {
		if err := r.DeleteRoom(roomName); err != nil {
			logger.Errorw("could not delete room", "error", err)
		}
	})
	r.lock.Lock()
	r.rooms[roomName] = room
	r.lock.Unlock()

	return room, nil
}

// manages a RTC session for a participant, runs on the RTC node
func (r *RoomManager) rtcSessionWorker(room *rtc.Room, participant types.Participant, requestSource routing.MessageSource) {
	defer func() {
		logger.Debugw("RTC session finishing",
			"participant", participant.Identity(),
			"room", room.Name,
		)
	}()
	defer rtc.Recover()

	for {
		select {
		case <-time.After(time.Millisecond * 100):
			// periodic check to ensure participant didn't become disconnected
			if participant.State() == livekit.ParticipantInfo_DISCONNECTED {
				return
			}
		case obj := <-requestSource.ReadChan():
			if obj == nil {
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
				if err := participant.HandleAnswer(sd); err != nil {
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
}
