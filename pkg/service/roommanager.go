package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/utils"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	livekit "github.com/livekit/livekit-server/proto"
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
	rtcConfig   *rtc.WebRTCConfig
	config      *config.Config
	rooms       map[string]*rtc.Room
}

func NewRoomManager(rp RoomStore, router routing.Router, currentNode routing.LocalNode, selector routing.NodeSelector, conf *config.Config) (*RoomManager, error) {
	rtcConf, err := rtc.NewWebRTCConfig(conf, currentNode.Ip)
	if err != nil {
		return nil, err
	}

	return &RoomManager{
		lock:        sync.RWMutex{},
		roomStore:   rp,
		rtcConfig:   rtcConf,
		config:      conf,
		router:      router,
		selector:    selector,
		currentNode: currentNode,
		rooms:       make(map[string]*rtc.Room),
	}, nil
}

// CreateRoom creates a new room from a request and allocates it to a node to handle
// it'll also monitor fits state, and cleans it up when appropriate
func (r *RoomManager) CreateRoom(req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	token, err := r.roomStore.LockRoom(req.Name, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = r.roomStore.UnlockRoom(req.Name, token)
	}()

	// find existing room and update it
	rm, err := r.roomStore.GetRoom(req.Name)
	if err == ErrRoomNotFound {
		rm = &livekit.Room{
			Sid:          utils.NewGuid(utils.RoomPrefix),
			Name:         req.Name,
			CreationTime: time.Now().Unix(),
			TurnPassword: utils.RandomSecret(),
		}
		applyDefaultRoomConfig(rm, &r.config.Room)
	} else if err != nil {
		return nil, err
	}

	if req.EmptyTimeout > 0 {
		rm.EmptyTimeout = req.EmptyTimeout
	}
	if req.MaxParticipants > 0 {
		rm.MaxParticipants = req.MaxParticipants
	}
	if err := r.roomStore.CreateRoom(rm); err != nil {
		return nil, err
	}

	// Is that node still available?
	node, err := r.router.GetNodeForRoom(rm.Name)
	if err != routing.ErrNotFound && err != nil {
		return nil, err
	}

	// keep it on that node
	if err == nil && routing.IsAvailable(node) {
		return rm, nil
	}

	// select a new node
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

func (r *RoomManager) GetRoom(roomName string) *rtc.Room {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.rooms[roomName]
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

	wg.Wait()
	if err != nil {
		err = err2
	}

	return err
}

// CleanupRooms cleans up after old rooms that have been around for awhile
func (r *RoomManager) CleanupRooms() error {
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

func (r *RoomManager) CloseIdleRooms() {
	r.lock.RLock()
	rooms := make([]*rtc.Room, 0, len(r.rooms))
	for _, rm := range r.rooms {
		rooms = append(rooms, rm)
	}
	r.lock.RUnlock()

	for _, room := range rooms {
		room.CloseIfEmpty()
	}
}

func (r *RoomManager) Stop() {
	// disconnect all clients
	r.lock.RLock()
	rooms := make([]*rtc.Room, 0, len(r.rooms))
	for _, rm := range r.rooms {
		rooms = append(rooms, rm)
	}
	r.lock.RUnlock()

	for _, room := range rooms {
		for _, p := range room.GetParticipants() {
			_ = p.Close()
		}
		room.Close()
	}

	if r.rtcConfig != nil {
		if r.rtcConfig.UDPMuxConn != nil {
			_ = r.rtcConfig.UDPMuxConn.Close()
		}
		if r.rtcConfig.TCPMuxListener != nil {
			_ = r.rtcConfig.TCPMuxListener.Close()
		}
	}
}

// StartSession starts WebRTC session when a new participant is connected, takes place on RTC node
func (r *RoomManager) StartSession(roomName string, pi routing.ParticipantInit, requestSource routing.MessageSource, responseSink routing.MessageSink) {
	room, err := r.getOrCreateRoom(roomName)
	if err != nil {
		logger.Errorw("could not create room", err)
		return
	}

	participant := room.GetParticipant(pi.Identity)
	if participant != nil {
		// When reconnecting, it means WS has interrupted by underlying peer connection is still ok
		// in this mode, we'll keep the participant SID, and just swap the sink for the underlying connection
		if pi.Reconnect {
			logger.Debugw("resuming RTC session",
				"room", roomName,
				"node", r.currentNode.Id,
				"participant", pi.Identity,
			)
			// close previous sink, and link to new one
			prevSink := participant.GetResponseSink()
			if prevSink != nil {
				prevSink.Close()
			}
			participant.SetResponseSink(responseSink)

			if err := participant.SendParticipantUpdate(rtc.ToProtoParticipants(room.GetParticipants())); err != nil {
				logger.Warnw("failed to send participant update", err,
					"participant", pi.Identity)
			}

			if err := participant.ICERestart(); err != nil {
				logger.Warnw("could not restart ICE", err,
					"participant", pi.Identity)
			}
			return
		} else {
			// we need to clean up the existing participant, so a new one can join
			room.RemoveParticipant(participant.Identity())
		}
	} else if pi.Reconnect {
		// send leave request if participant is trying to reconnect but missing from the room
		if err = responseSink.WriteMessage(&livekit.SignalRequest{
			Message: &livekit.SignalRequest_Leave{
				Leave: &livekit.LeaveRequest{
					CanReconnect: true,
				},
			},
		}); err != nil {
			logger.Warnw("could not restart participant", err,
				"participant", pi.Identity)
		}
		return
	}

	logger.Debugw("starting RTC session",
		"room", roomName,
		"node", r.currentNode.Id,
		"participant", pi.Identity,
		"plan_b", pi.UsePlanB,
		"protocol", pi.ProtocolVersion,
	)

	pv := types.ProtocolVersion(pi.ProtocolVersion)
	rtcConf := *r.rtcConfig
	rtcConf.SetBufferFactory(room.GetBufferFactor())
	if pi.UsePlanB {
		rtcConf.Configuration.SDPSemantics = webrtc.SDPSemanticsPlanB
	}
	participant, err = rtc.NewParticipant(rtc.ParticipantParams{
		Identity:        pi.Identity,
		Config:          &rtcConf,
		Sink:            responseSink,
		AudioConfig:     r.config.Audio,
		ProtocolVersion: pv,
		Stats:           room.GetStatsReporter(),
		ThrottleConfig:  r.config.RTC.PLIThrottle,
		EnabledCodecs:   room.Room.EnabledCodecs,
	})
	if err != nil {
		logger.Errorw("could not create participant", err)
		return
	}
	if pi.Metadata != "" {
		participant.SetMetadata(pi.Metadata)
	}

	if pi.Permission != nil {
		participant.SetPermission(pi.Permission)
	}

	// join room
	opts := rtc.ParticipantOptions{
		AutoSubscribe: pi.AutoSubscribe,
	}
	if err := room.Join(participant, &opts); err != nil {
		logger.Errorw("could not join room", err)
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

	// construct ice servers
	room = rtc.NewRoom(ri, *r.rtcConfig, r.iceServersForRoom(ri), &r.config.Audio)
	room.OnClose(func() {
		if err := r.DeleteRoom(roomName); err != nil {
			logger.Errorw("could not delete room", err)
		}

		// print stats
		logger.Infow("room closed",
			"incomingStats", room.GetIncomingStats().Copy(),
			"outgoingStats", room.GetOutgoingStats().Copy(),
		)
	})
	room.OnParticipantChanged(func(p types.Participant) {
		var err error
		if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			err = r.roomStore.DeleteParticipant(roomName, p.Identity())
		} else {
			err = r.roomStore.PersistParticipant(roomName, p.ToProto())
		}
		if err != nil {
			logger.Errorw("could not handle participant change", err)
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
			"room", room.Room.Name,
		)
		_ = participant.Close()
	}()
	defer rtc.Recover()

	for {
		select {
		case <-time.After(time.Millisecond * 50):
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
				_, err := participant.HandleOffer(rtc.FromProtoSessionDescription(msg.Offer))
				if err != nil {
					logger.Errorw("could not handle offer", err, "participant", participant.Identity())
					return
				}
			case *livekit.SignalRequest_AddTrack:
				logger.Debugw("add track request", "participant", participant.Identity(),
					"track", msg.AddTrack.Cid)
				participant.AddTrack(msg.AddTrack)
			case *livekit.SignalRequest_Answer:
				if participant.State() == livekit.ParticipantInfo_JOINING {
					logger.Errorw("cannot negotiate before peer offer", nil, "participant", participant.Identity())
					// conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot negotiate before peer offer"))
					return
				}
				sd := rtc.FromProtoSessionDescription(msg.Answer)
				if err := participant.HandleAnswer(sd); err != nil {
					logger.Errorw("could not handle answer", err, "participant", participant.Identity())
				}
			case *livekit.SignalRequest_Trickle:
				candidateInit, err := rtc.FromProtoTrickle(msg.Trickle)
				if err != nil {
					logger.Errorw("could not decode trickle", err, "participant", participant.Identity())
					break
				}
				// logger.Debugw("adding peer candidate", "participant", participant.ID())
				if err := participant.AddICECandidate(candidateInit, msg.Trickle.Target); err != nil {
					logger.Errorw("could not handle trickle", err, "participant", participant.Identity())
				}
			case *livekit.SignalRequest_Mute:
				participant.SetTrackMuted(msg.Mute.Sid, msg.Mute.Muted)
			case *livekit.SignalRequest_Subscription:
				if err := room.UpdateSubscriptions(participant, msg.Subscription.TrackSids, msg.Subscription.Subscribe); err != nil {
					logger.Warnw("could not update subscription", err,
						"participant", participant.Identity(),
						"tracks", msg.Subscription.TrackSids,
						"subscribe", msg.Subscription.Subscribe)
				}
			case *livekit.SignalRequest_TrackSetting:
				for _, subTrack := range participant.GetSubscribedTracks() {
					for _, sid := range msg.TrackSetting.TrackSids {
						if subTrack.ID() != sid {
							continue
						}
						logger.Debugw("updating track settings",
							"participant", participant.Identity(),
							"settings", msg.TrackSetting)
						subTrack.UpdateSubscriberSettings(!msg.TrackSetting.Disabled, msg.TrackSetting.Quality)
					}
				}
			case *livekit.SignalRequest_Leave:
				_ = participant.Close()
			}
		}
	}
}

func (r *RoomManager) handleRTCMessage(roomName, identity string, msg *livekit.RTCNodeMessage) {
	r.lock.RLock()
	room := r.rooms[roomName]
	r.lock.RUnlock()

	if room == nil {
		logger.Warnw("Could not find room", nil, "room", roomName)
		return
	}

	participant := room.GetParticipant(identity)
	if participant == nil {
		return
	}

	switch rm := msg.Message.(type) {
	case *livekit.RTCNodeMessage_RemoveParticipant:
		logger.Infow("removing participant", "room", roomName, "participant", identity)
		room.RemoveParticipant(identity)
	case *livekit.RTCNodeMessage_MuteTrack:
		logger.Debugw("setting track muted", "room", roomName, "participant", identity,
			"track", rm.MuteTrack.TrackSid, "muted", rm.MuteTrack.Muted)
		participant.SetTrackMuted(rm.MuteTrack.TrackSid, rm.MuteTrack.Muted)
	case *livekit.RTCNodeMessage_UpdateParticipant:
		logger.Debugw("updating participant", "room", roomName, "participant", identity)
		if rm.UpdateParticipant.Metadata != "" {
			participant.SetMetadata(rm.UpdateParticipant.Metadata)
		}
		if rm.UpdateParticipant.Permission != nil {
			participant.SetPermission(rm.UpdateParticipant.Permission)
		}
	case *livekit.RTCNodeMessage_DeleteRoom:
		for _, p := range room.GetParticipants() {
			_ = p.Close()
		}
		room.Close()
	case *livekit.RTCNodeMessage_UpdateSubscriptions:
		logger.Debugw("updating participant subscriptions", "room", roomName, "participant", identity)
		if err := room.UpdateSubscriptions(participant, rm.UpdateSubscriptions.TrackSids, rm.UpdateSubscriptions.Subscribe); err != nil {
			logger.Warnw("could not update subscription", err,
				"participant", participant.Identity(),
				"tracks", rm.UpdateSubscriptions.TrackSids,
				"subscribe", rm.UpdateSubscriptions.Subscribe)
		}
	}
}

func (r *RoomManager) iceServersForRoom(ri *livekit.Room) []*livekit.ICEServer {
	var iceServers []*livekit.ICEServer

	if len(r.rtcConfig.Configuration.ICEServers) > 0 {
		iceServers = append(iceServers, &livekit.ICEServer{
			Urls: r.rtcConfig.Configuration.ICEServers[0].URLs,
		})
	}
	if r.config.TURN.Enabled {
		iceServers = append(iceServers, &livekit.ICEServer{
			Urls:       []string{fmt.Sprintf("turns:%s:443?transport=tcp", r.config.TURN.Domain)},
			Username:   ri.Name,
			Credential: ri.TurnPassword,
		})
	}
	return iceServers
}

func applyDefaultRoomConfig(room *livekit.Room, conf *config.RoomConfig) {
	room.EmptyTimeout = conf.EmptyTimeout
	room.MaxParticipants = conf.MaxParticipants
	for _, codec := range conf.EnabledCodecs {
		room.EnabledCodecs = append(room.EnabledCodecs, &livekit.Codec{
			Mime:     codec.Mime,
			FmtpLine: codec.FmtpLine,
		})
	}
}
