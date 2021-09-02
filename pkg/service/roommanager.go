package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gammazero/workerpool"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

const (
	roomPurgeSeconds = 24 * 60 * 60
)

// LocalRoomManager manages rooms and its interaction with participants.
// It's responsible for creating, deleting rooms, as well as running sessions for participants
type LocalRoomManager struct {
	RoomStore

	lock        sync.RWMutex
	selector    routing.NodeSelector
	router      routing.Router
	currentNode routing.LocalNode
	notifier    *webhook.Notifier
	rtcConfig   *rtc.WebRTCConfig
	config      *config.Config
	webhookPool *workerpool.WorkerPool
	rooms       map[string]*rtc.Room
}

func NewLocalRoomManager(rp RoomStore, router routing.Router, currentNode routing.LocalNode, selector routing.NodeSelector,
	notifier *webhook.Notifier, conf *config.Config) (*LocalRoomManager, error) {
	rtcConf, err := rtc.NewWebRTCConfig(conf, currentNode.Ip)
	if err != nil {
		return nil, err
	}

	r := &LocalRoomManager{
		RoomStore:   rp,
		lock:        sync.RWMutex{},
		rtcConfig:   rtcConf,
		config:      conf,
		router:      router,
		selector:    selector,
		notifier:    notifier,
		currentNode: currentNode,
		webhookPool: workerpool.New(1),
		rooms:       make(map[string]*rtc.Room),
	}

	// hook up to router
	router.OnNewParticipantRTC(r.StartSession)
	router.OnRTCMessage(r.handleRTCMessage)
	return r, nil
}

// CreateRoom creates a new room from a request and allocates it to a node to handle
// it'll also monitor fits state, and cleans it up when appropriate
func (r *LocalRoomManager) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	token, err := r.LockRoom(ctx, req.Name, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = r.UnlockRoom(ctx, req.Name, token)
	}()

	// find existing room and update it
	rm, err := r.LoadRoom(ctx, req.Name)
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
	if err := r.StoreRoom(ctx, rm); err != nil {
		return nil, err
	}

	// Is that node still available?
	node, err := r.router.GetNodeForRoom(ctx, rm.Name)
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

	logger.Debugw("selected node for room", "room", rm.Name, "roomID", rm.Sid, "nodeID", nodeId)
	if err := r.router.SetNodeForRoom(ctx, req.Name, nodeId); err != nil {
		return nil, err
	}

	return rm, nil
}

func (r *LocalRoomManager) GetRoom(ctx context.Context, roomName string) *rtc.Room {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.rooms[roomName]
}

// DeleteRoom completely deletes all room information, including active sessions, room store, and routing info
func (r *LocalRoomManager) DeleteRoom(ctx context.Context, roomName string) error {
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
		err = r.router.ClearRoomState(ctx, roomName)
	}()
	// also delete room from db
	go func() {
		defer wg.Done()
		err2 = r.RoomStore.DeleteRoom(ctx, roomName)
	}()

	wg.Wait()
	if err2 != nil {
		err = err2
	}

	return err
}

// CleanupRooms cleans up after old rooms that have been around for awhile
func (r *LocalRoomManager) CleanupRooms() error {
	// cleanup rooms that have been left for over a day
	ctx := context.Background()
	rooms, err := r.ListRooms(ctx)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	for _, room := range rooms {
		if (now - room.CreationTime) > roomPurgeSeconds {
			if err := r.DeleteRoom(ctx, room.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *LocalRoomManager) CloseIdleRooms() {
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

func (r *LocalRoomManager) Stop() {
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
func (r *LocalRoomManager) StartSession(ctx context.Context, roomName string, pi routing.ParticipantInit, requestSource routing.MessageSource, responseSink routing.MessageSink) {
	room, err := r.getOrCreateRoom(ctx, roomName)
	if err != nil {
		logger.Errorw("could not create room", err, "room", roomName)
		return
	}

	participant := room.GetParticipant(pi.Identity)
	if participant != nil {
		// When reconnecting, it means WS has interrupted by underlying peer connection is still ok
		// in this mode, we'll keep the participant SID, and just swap the sink for the underlying connection
		if pi.Reconnect {
			logger.Debugw("resuming RTC session",
				"room", roomName,
				"nodeID", r.currentNode.Id,
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
		if err = responseSink.WriteMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{
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
		"nodeID", r.currentNode.Id,
		"participant", pi.Identity,
		"protocol", pi.ProtocolVersion,
	)

	pv := types.ProtocolVersion(pi.ProtocolVersion)
	rtcConf := *r.rtcConfig
	rtcConf.SetBufferFactory(room.GetBufferFactor())
	participant, err = rtc.NewParticipant(rtc.ParticipantParams{
		Identity:        pi.Identity,
		Config:          &rtcConf,
		Sink:            responseSink,
		AudioConfig:     r.config.Audio,
		ProtocolVersion: pv,
		Stats:           room.GetStatsReporter(),
		ThrottleConfig:  r.config.RTC.PLIThrottle,
		EnabledCodecs:   room.Room.EnabledCodecs,
		Hidden:          pi.Hidden,
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

// create the actual room object, to be used on RTC node
func (r *LocalRoomManager) getOrCreateRoom(ctx context.Context, roomName string) (*rtc.Room, error) {
	r.lock.RLock()
	room := r.rooms[roomName]
	r.lock.RUnlock()

	if room != nil {
		return room, nil
	}

	// create new room, get details first
	ri, err := r.LoadRoom(ctx, roomName)
	if err != nil {
		return nil, err
	}

	// construct ice servers
	room = rtc.NewRoom(ri, *r.rtcConfig, r.iceServersForRoom(ri), &r.config.Audio)
	room.OnClose(func() {
		if err := r.DeleteRoom(ctx, roomName); err != nil {
			logger.Errorw("could not delete room", err)
		}

		r.notifyEvent(&livekit.WebhookEvent{
			Event: webhook.EventRoomFinished,
			Room:  room.Room,
		})

		// print stats
		logger.Infow("room closed",
			"incomingStats", room.GetIncomingStats().Copy(),
			"outgoingStats", room.GetOutgoingStats().Copy(),
		)
	})
	room.OnParticipantChanged(func(p types.Participant) {
		var err error
		if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			err = r.DeleteParticipant(ctx, roomName, p.Identity())
		} else {
			err = r.StoreParticipant(ctx, roomName, p.ToProto())
		}
		if err != nil {
			logger.Errorw("could not handle participant change", err)
		}
	})
	r.lock.Lock()
	r.rooms[roomName] = room
	r.lock.Unlock()

	r.notifyEvent(&livekit.WebhookEvent{
		Event: webhook.EventRoomStarted,
		Room:  room.Room,
	})

	return room, nil
}

// manages an RTC session for a participant, runs on the RTC node
func (r *LocalRoomManager) rtcSessionWorker(room *rtc.Room, participant types.Participant, requestSource routing.MessageSource) {
	defer func() {
		logger.Debugw("RTC session finishing",
			"participant", participant.Identity(),
			"pID", participant.ID(),
			"room", room.Room.Name,
			"roomID", room.Room.Sid,
		)
		_ = participant.Close()

		r.notifyEvent(&livekit.WebhookEvent{
			Event:       webhook.EventParticipantLeft,
			Room:        room.Room,
			Participant: participant.ToProto(),
		})
	}()
	defer rtc.Recover()

	r.notifyEvent(&livekit.WebhookEvent{
		Event:       webhook.EventParticipantJoined,
		Room:        room.Room,
		Participant: participant.ToProto(),
	})
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
					logger.Errorw("could not handle offer", err, "participant", participant.Identity(), "pID", participant.ID())
					return
				}
			case *livekit.SignalRequest_AddTrack:
				logger.Debugw("add track request", "participant", participant.Identity(), "pID", participant.ID(),
					"track", msg.AddTrack.Cid)
				participant.AddTrack(msg.AddTrack)
			case *livekit.SignalRequest_Answer:
				if participant.State() == livekit.ParticipantInfo_JOINING {
					logger.Errorw("cannot negotiate before peer offer", nil, "participant", participant.Identity(), "pID", participant.ID())
					// conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot negotiate before peer offer"))
					return
				}
				sd := rtc.FromProtoSessionDescription(msg.Answer)
				if err := participant.HandleAnswer(sd); err != nil {
					logger.Errorw("could not handle answer", err, "participant", participant.Identity(), "pID", participant.ID())
				}
			case *livekit.SignalRequest_Trickle:
				candidateInit, err := rtc.FromProtoTrickle(msg.Trickle)
				if err != nil {
					logger.Errorw("could not decode trickle", err, "participant", participant.Identity(), "pID", participant.ID())
					break
				}
				// logger.Debugw("adding peer candidate", "participant", participant.Identity())
				if err := participant.AddICECandidate(candidateInit, msg.Trickle.Target); err != nil {
					logger.Errorw("could not handle trickle", err, "participant", participant.Identity(), "pID", participant.ID())
				}
			case *livekit.SignalRequest_Mute:
				participant.SetTrackMuted(msg.Mute.Sid, msg.Mute.Muted, false)
			case *livekit.SignalRequest_Subscription:
				if err := room.UpdateSubscriptions(participant, msg.Subscription.TrackSids, msg.Subscription.Subscribe); err != nil {
					logger.Warnw("could not update subscription", err,
						"participant", participant.Identity(),
						"pID", participant.ID(),
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
							"pID", participant.ID(),
							"settings", msg.TrackSetting)
						subTrack.UpdateSubscriberSettings(!msg.TrackSetting.Disabled, msg.TrackSetting.Quality)
					}
				}
			case *livekit.SignalRequest_Leave:
				_ = participant.Close()
			case *livekit.SignalRequest_Simulcast:
				// deprecated
			}
		}
	}
}

// handles RTC messages resulted from Room API calls
func (r *LocalRoomManager) handleRTCMessage(ctx context.Context, roomName, identity string, msg *livekit.RTCNodeMessage) {
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
		if !rm.MuteTrack.Muted && !r.config.Room.EnableRemoteUnmute {
			logger.Errorw("cannot unmute track, remote unmute is disabled", nil)
			return
		}
		participant.SetTrackMuted(rm.MuteTrack.TrackSid, rm.MuteTrack.Muted, true)
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
				"pID", participant.ID(),
				"tracks", rm.UpdateSubscriptions.TrackSids,
				"subscribe", rm.UpdateSubscriptions.Subscribe)
		}
	case *livekit.RTCNodeMessage_SendData:
		logger.Debugw("SendData", "message", rm)
		up := &livekit.UserPacket{
			Payload:         rm.SendData.Data,
			DestinationSids: rm.SendData.DestinationSids,
		}
		room.SendDataPacket(up, rm.SendData.Kind)
	}
}

func (r *LocalRoomManager) iceServersForRoom(ri *livekit.Room) []*livekit.ICEServer {
	var iceServers []*livekit.ICEServer

	hasSTUN := false
	if r.config.TURN.Enabled {
		var urls []string
		if r.config.TURN.UDPPort > 0 {
			// UDP TURN is used as STUN
			hasSTUN = true
			urls = append(urls, fmt.Sprintf("turn:%s:%d?transport=udp", r.config.RTC.NodeIP, r.config.TURN.UDPPort))
		}
		if r.config.TURN.TLSPort > 0 {
			urls = append(urls, fmt.Sprintf("turns:%s:443?transport=tcp", r.config.TURN.Domain))
		}
		if len(urls) > 0 {
			iceServers = append(iceServers, &livekit.ICEServer{
				Urls:       urls,
				Username:   ri.Name,
				Credential: ri.TurnPassword,
			})
		}
	}

	if len(r.config.RTC.StunServers) > 0 {
		hasSTUN = true
		iceServers = append(iceServers, iceServerForStunServers(r.config.RTC.StunServers))
	}

	if !hasSTUN {
		iceServers = append(iceServers, iceServerForStunServers(config.DefaultStunServers))
	}
	return iceServers
}

func (r *LocalRoomManager) notifyEvent(event *livekit.WebhookEvent) {
	if r.notifier == nil {
		return
	}

	r.webhookPool.Submit(func() {
		if err := r.notifier.Notify(event); err != nil {
			logger.Warnw("could not notify webhook", err, "event", event.Event)
		}
	})
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

func iceServerForStunServers(servers []string) *livekit.ICEServer {
	iceServer := &livekit.ICEServer{}
	for _, stunServer := range servers {
		iceServer.Urls = append(iceServer.Urls, fmt.Sprintf("stun:%s", stunServer))
	}
	return iceServer
}
