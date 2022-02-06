package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

const (
	roomPurgeSeconds     = 24 * 60 * 60
	tokenRefreshInterval = 5 * time.Minute
	tokenDefaultTTL      = 10 * time.Minute
)

// RoomManager manages rooms and its interaction with participants.
// It's responsible for creating, deleting rooms, as well as running sessions for participants
type RoomManager struct {
	lock sync.RWMutex

	config      *config.Config
	rtcConfig   *rtc.WebRTCConfig
	currentNode routing.LocalNode
	router      routing.Router
	roomStore   RoomStore
	telemetry   telemetry.TelemetryService

	rooms map[livekit.RoomName]*rtc.Room
}

func NewLocalRoomManager(
	conf *config.Config,
	roomStore RoomStore,
	currentNode routing.LocalNode,
	router routing.Router,
	telemetry telemetry.TelemetryService,
) (*RoomManager, error) {

	rtcConf, err := rtc.NewWebRTCConfig(conf, currentNode.Ip)
	if err != nil {
		return nil, err
	}

	r := &RoomManager{
		config:      conf,
		rtcConfig:   rtcConf,
		currentNode: currentNode,
		router:      router,
		roomStore:   roomStore,
		telemetry:   telemetry,

		rooms: make(map[livekit.RoomName]*rtc.Room),
	}

	// hook up to router
	router.OnNewParticipantRTC(r.StartSession)
	router.OnRTCMessage(r.handleRTCMessage)
	return r, nil
}

func (r *RoomManager) GetRoom(_ context.Context, roomName livekit.RoomName) *rtc.Room {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.rooms[roomName]
}

// DeleteRoom completely deletes all room information, including active sessions, room store, and routing info
func (r *RoomManager) DeleteRoom(ctx context.Context, roomName livekit.RoomName) error {
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
		err2 = r.roomStore.DeleteRoom(ctx, roomName)
	}()

	wg.Wait()
	if err2 != nil {
		err = err2
	}

	return err
}

// CleanupRooms cleans up after old rooms that have been around for a while
func (r *RoomManager) CleanupRooms() error {
	// cleanup rooms that have been left for over a day
	ctx := context.Background()
	rooms, err := r.roomStore.ListRooms(ctx, nil)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	for _, room := range rooms {
		if (now - room.CreationTime) > roomPurgeSeconds {
			if err := r.DeleteRoom(ctx, livekit.RoomName(room.Name)); err != nil {
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

func (r *RoomManager) HasParticipants() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()

	for _, room := range r.rooms {
		if len(room.GetParticipants()) != 0 {
			return true
		}
	}
	return false
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
			_ = p.Close(true)
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
func (r *RoomManager) StartSession(ctx context.Context, roomName livekit.RoomName, pi routing.ParticipantInit, requestSource routing.MessageSource, responseSink routing.MessageSink) {
	room, err := r.getOrCreateRoom(ctx, roomName)
	if err != nil {
		logger.Errorw("could not create room", err, "room", roomName)
		return
	}
	defer room.Release()

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
			if err = room.ResumeParticipant(participant, responseSink); err != nil {
				logger.Warnw("could not resume participant", err,
					"participant", pi.Identity)
			}
			return
		} else {
			// we need to clean up the existing participant, so a new one can join
			room.RemoveParticipant(participant.Identity())
		}
	} else if pi.Reconnect {
		// send leave request if participant is trying to reconnect without keep subscribe state
		// but missing from the room
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
		"sdk", pi.Client.Sdk,
		"sdkVersion", pi.Client.Version,
		"protocol", pi.Client.Protocol,
	)

	pv := types.ProtocolVersion(pi.Client.Protocol)
	rtcConf := *r.rtcConfig
	rtcConf.SetBufferFactory(room.GetBufferFactory())
	sid := livekit.ParticipantID(utils.NewGuid(utils.ParticipantPrefix))
	pLogger := rtc.LoggerWithParticipant(room.Logger, pi.Identity, sid)
	participant, err = rtc.NewParticipant(rtc.ParticipantParams{
		Identity:                pi.Identity,
		Name:                    pi.Name,
		SID:                     sid,
		Config:                  &rtcConf,
		Sink:                    responseSink,
		AudioConfig:             r.config.Audio,
		ProtocolVersion:         pv,
		Telemetry:               r.telemetry,
		ThrottleConfig:          r.config.RTC.PLIThrottle,
		CongestionControlConfig: r.config.RTC.CongestionControl,
		EnabledCodecs:           room.Room.EnabledCodecs,
		Grants:                  pi.Grants,
		Hidden:                  pi.Hidden,
		Logger:                  pLogger,
	}, pi.Permission)
	if err != nil {
		logger.Errorw("could not create participant", err)
		return
	}

	// join room
	opts := rtc.ParticipantOptions{
		AutoSubscribe: pi.AutoSubscribe,
	}
	if err = room.Join(participant, &opts, r.iceServersForRoom(room.Room)); err != nil {
		pLogger.Errorw("could not join room", err)
		return
	}
	if err = r.roomStore.StoreParticipant(ctx, roomName, participant.ToProto()); err != nil {
		pLogger.Errorw("could not store participant", err)
	}

	updateParticipantCount := func() {
		if !participant.Hidden() {
			err = r.roomStore.StoreRoom(ctx, room.Room)
			if err != nil {
				logger.Errorw("could not store room", err)
			}
		}
	}

	// update room store with new numParticipants
	updateParticipantCount()

	clientMeta := &livekit.AnalyticsClientMeta{Region: r.currentNode.Region, Node: r.currentNode.Id}
	r.telemetry.ParticipantJoined(ctx, room.Room, participant.ToProto(), pi.Client, clientMeta)
	participant.OnClose(func(p types.LocalParticipant, disallowedSubscriptions map[livekit.TrackID]livekit.ParticipantID) {
		if err := r.roomStore.DeleteParticipant(ctx, roomName, p.Identity()); err != nil {
			pLogger.Errorw("could not delete participant", err)
		}

		// update room store with new numParticipants
		updateParticipantCount()
		r.telemetry.ParticipantLeft(ctx, room.Room, p.ToProto())

		room.RemoveDisallowedSubscriptions(p, disallowedSubscriptions)
	})
	participant.OnClaimsChanged(func(participant types.LocalParticipant) {
		pLogger.Debugw("refreshing client token after claims change")
		if err := r.refreshToken(participant); err != nil {
			logger.Errorw("could not refresh token", err)
		}
	})

	go r.rtcSessionWorker(room, participant, requestSource)
}

// create the actual room object, to be used on RTC node
func (r *RoomManager) getOrCreateRoom(ctx context.Context, roomName livekit.RoomName) (*rtc.Room, error) {
	r.lock.RLock()
	room := r.rooms[roomName]
	r.lock.RUnlock()

	if room != nil && room.Hold() {
		return room, nil
	}

	// create new room, get details first
	ri, err := r.roomStore.LoadRoom(ctx, roomName)
	if err != nil {
		return nil, err
	}

	// construct ice servers
	room = rtc.NewRoom(ri, *r.rtcConfig, &r.config.Audio, r.telemetry)
	room.Hold()

	r.telemetry.RoomStarted(ctx, room.Room)

	room.OnClose(func() {
		r.telemetry.RoomEnded(ctx, room.Room)
		if err := r.DeleteRoom(ctx, roomName); err != nil {
			logger.Errorw("could not delete room", err)
		}

		logger.Infow("room closed")
	})

	room.OnMetadataUpdate(func(metadata string) {
		if err := r.roomStore.StoreRoom(ctx, room.Room); err != nil {
			logger.Errorw("could not handle metadata update", err)
		}
	})

	room.OnParticipantChanged(func(p types.LocalParticipant) {
		if p.State() != livekit.ParticipantInfo_DISCONNECTED {
			if err := r.roomStore.StoreParticipant(ctx, roomName, p.ToProto()); err != nil {
				logger.Errorw("could not handle participant change", err)
			}
		}
	})

	r.lock.Lock()
	r.rooms[roomName] = room
	r.lock.Unlock()

	return room, nil
}

// manages an RTC session for a participant, runs on the RTC node
func (r *RoomManager) rtcSessionWorker(room *rtc.Room, participant types.LocalParticipant, requestSource routing.MessageSource) {
	defer func() {
		logger.Debugw("RTC session finishing",
			"participant", participant.Identity(),
			"pID", participant.ID(),
			"room", room.Room.Name,
			"roomID", room.Room.Sid,
		)
		_ = participant.Close(true)
	}()
	defer rtc.Recover()

	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.Logger(logger.GetLogger()), room.Name(), room.ID()),
		participant.Identity(), participant.ID(),
	)

	lastTokenUpdate := time.Now()
	for {
		select {
		case <-time.After(time.Millisecond * 50):
			// periodic check to ensure participant didn't become disconnected
			if participant.State() == livekit.ParticipantInfo_DISCONNECTED {
				return
			}

			if time.Now().Sub(lastTokenUpdate) > tokenRefreshInterval {
				pLogger.Debugw("refreshing client token after interval")
				// refresh token with the first API Key/secret pair
				if err := r.refreshToken(participant); err != nil {
					pLogger.Errorw("could not refresh token", err)
				}
				lastTokenUpdate = time.Now()
			}
		case obj := <-requestSource.ReadChan():
			// In single node mode, the request source is directly tied to the signal message channel
			// this means ICE restart isn't possible in single node mode
			if obj == nil {
				return
			}

			req := obj.(*livekit.SignalRequest)
			if err := rtc.HandleParticipantSignal(room, participant, req, pLogger); err != nil {
				// more specific errors are already logged
				// treat errors returned as fatal
				return
			}
		}
	}
}

// handles RTC messages resulted from Room API calls
func (r *RoomManager) handleRTCMessage(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) {
	r.lock.RLock()
	room := r.rooms[roomName]
	r.lock.RUnlock()

	if room == nil {
		logger.Warnw("Could not find room", nil, "room", roomName)
		return
	}

	participant := room.GetParticipant(identity)
	var sid livekit.ParticipantID
	if participant != nil {
		sid = participant.ID()
	}
	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.Logger(logger.GetLogger()), roomName, room.ID()),
		identity,
		sid,
	)

	switch rm := msg.Message.(type) {
	case *livekit.RTCNodeMessage_RemoveParticipant:
		if participant == nil {
			return
		}
		pLogger.Infow("removing participant")
		room.RemoveParticipant(identity)
	case *livekit.RTCNodeMessage_MuteTrack:
		if participant == nil {
			return
		}
		pLogger.Debugw("setting track muted",
			"track", rm.MuteTrack.TrackSid, "muted", rm.MuteTrack.Muted)
		if !rm.MuteTrack.Muted && !r.config.Room.EnableRemoteUnmute {
			pLogger.Errorw("cannot unmute track, remote unmute is disabled", nil)
			return
		}
		participant.SetTrackMuted(livekit.TrackID(rm.MuteTrack.TrackSid), rm.MuteTrack.Muted, true)
	case *livekit.RTCNodeMessage_UpdateParticipant:
		if participant == nil {
			return
		}
		pLogger.Debugw("updating participant")
		if rm.UpdateParticipant.Metadata != "" {
			participant.SetMetadata(rm.UpdateParticipant.Metadata)
		}
		if rm.UpdateParticipant.Permission != nil {
			err := room.SetParticipantPermission(participant, rm.UpdateParticipant.Permission)
			if err != nil {
				pLogger.Errorw("could not update permissions", err)
			}
		}
	case *livekit.RTCNodeMessage_DeleteRoom:
		for _, p := range room.GetParticipants() {
			_ = p.Close(true)
		}
		room.Close()
	case *livekit.RTCNodeMessage_UpdateSubscriptions:
		if participant == nil {
			return
		}
		pLogger.Debugw("updating participant subscriptions")
		if err := room.UpdateSubscriptions(
			participant,
			livekit.StringsAsTrackIDs(rm.UpdateSubscriptions.TrackSids),
			rm.UpdateSubscriptions.ParticipantTracks,
			rm.UpdateSubscriptions.Subscribe,
		); err != nil {
			pLogger.Warnw("could not update subscription", err,
				"tracks", rm.UpdateSubscriptions.TrackSids,
				"subscribe", rm.UpdateSubscriptions.Subscribe)
		}
	case *livekit.RTCNodeMessage_SendData:
		pLogger.Debugw("SendData", "size", len(rm.SendData.Data))
		up := &livekit.UserPacket{
			Payload:         rm.SendData.Data,
			DestinationSids: rm.SendData.DestinationSids,
		}
		room.SendDataPacket(up, rm.SendData.Kind)
	case *livekit.RTCNodeMessage_UpdateRoomMetadata:
		pLogger.Debugw("updating room")
		room.SetMetadata(rm.UpdateRoomMetadata.Metadata)
	}
}

func (r *RoomManager) iceServersForRoom(ri *livekit.Room) []*livekit.ICEServer {
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

	if len(r.config.RTC.STUNServers) > 0 {
		hasSTUN = true
		iceServers = append(iceServers, iceServerForStunServers(r.config.RTC.STUNServers))
	}

	if !hasSTUN {
		iceServers = append(iceServers, iceServerForStunServers(config.DefaultStunServers))
	}
	return iceServers
}

func (r *RoomManager) refreshToken(participant types.LocalParticipant) error {
	for key, secret := range r.config.Keys {
		grants := participant.ClaimGrants()
		token := auth.NewAccessToken(key, secret)
		token.SetName(grants.Name).
			SetIdentity(string(participant.Identity())).
			SetValidFor(tokenDefaultTTL).
			SetMetadata(grants.Metadata).
			AddGrant(grants.Video)
		jwt, err := token.ToJWT()
		if err == nil {
			err = participant.SendRefreshToken(jwt)
		}
		if err != nil {
			return err
		}
		break
	}
	return nil
}

func iceServerForStunServers(servers []string) *livekit.ICEServer {
	iceServer := &livekit.ICEServer{}
	for _, stunServer := range servers {
		iceServer.Urls = append(iceServer.Urls, fmt.Sprintf("stun:%s", stunServer))
	}
	return iceServer
}
