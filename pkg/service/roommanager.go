package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/version"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/clientconfiguration"
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
	iceConfigTTL         = 5 * time.Minute
)

type iceConfigCacheEntry struct {
	iceConfig  *livekit.ICEConfig
	modifiedAt time.Time
}

// RoomManager manages rooms and its interaction with participants.
// It's responsible for creating, deleting rooms, as well as running sessions for participants
type RoomManager struct {
	lock sync.RWMutex

	config            *config.Config
	rtcConfig         *rtc.WebRTCConfig
	serverInfo        *livekit.ServerInfo
	currentNode       routing.LocalNode
	router            routing.Router
	roomStore         ObjectStore
	telemetry         telemetry.TelemetryService
	clientConfManager clientconfiguration.ClientConfigurationManager
	egressLauncher    rtc.EgressLauncher
	versionGenerator  utils.TimedVersionGenerator

	rooms map[livekit.RoomName]*rtc.Room

	iceConfigCache map[livekit.ParticipantIdentity]*iceConfigCacheEntry
}

func NewLocalRoomManager(
	conf *config.Config,
	roomStore ObjectStore,
	currentNode routing.LocalNode,
	router routing.Router,
	telemetry telemetry.TelemetryService,
	clientConfManager clientconfiguration.ClientConfigurationManager,
	egressLauncher rtc.EgressLauncher,
	versionGenerator utils.TimedVersionGenerator,
) (*RoomManager, error) {
	rtcConf, err := rtc.NewWebRTCConfig(conf, currentNode.Ip)
	if err != nil {
		return nil, err
	}

	r := &RoomManager{
		config:            conf,
		rtcConfig:         rtcConf,
		currentNode:       currentNode,
		router:            router,
		roomStore:         roomStore,
		telemetry:         telemetry,
		clientConfManager: clientConfManager,
		egressLauncher:    egressLauncher,
		versionGenerator:  versionGenerator,

		rooms: make(map[livekit.RoomName]*rtc.Room),

		iceConfigCache: make(map[livekit.ParticipantIdentity]*iceConfigCacheEntry),

		serverInfo: &livekit.ServerInfo{
			Edition:  livekit.ServerInfo_Standard,
			Version:  version.Version,
			Protocol: types.CurrentProtocol,
			Region:   conf.Region,
			NodeId:   currentNode.Id,
		},
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
			_ = p.Close(true, types.ParticipantCloseReasonRoomManagerStop)
		}
		room.Close()
	}

	if r.rtcConfig != nil {
		if r.rtcConfig.UDPMux != nil {
			_ = r.rtcConfig.UDPMux.Close()
		}
		if r.rtcConfig.TCPMuxListener != nil {
			_ = r.rtcConfig.TCPMuxListener.Close()
		}
	}
}

// StartSession starts WebRTC session when a new participant is connected, takes place on RTC node
func (r *RoomManager) StartSession(
	ctx context.Context,
	roomName livekit.RoomName,
	pi routing.ParticipantInit,
	requestSource routing.MessageSource,
	responseSink routing.MessageSink,
) error {
	room, err := r.getOrCreateRoom(ctx, roomName)
	if err != nil {
		return err
	}
	defer room.Release()

	protoRoom := room.ToProto()

	// only create the room, but don't start a participant session
	if pi.Identity == "" {
		return nil
	}
	participant := room.GetParticipant(pi.Identity)
	if participant != nil {
		// When reconnecting, it means WS has interrupted by underlying peer connection is still ok
		// in this mode, we'll keep the participant SID, and just swap the sink for the underlying connection
		if pi.Reconnect {
			logger.Infow("resuming RTC session",
				"room", roomName,
				"nodeID", r.currentNode.Id,
				"participant", pi.Identity,
				"reason", pi.ReconnectReason,
			)
			iceConfig := r.getIceConfig(participant)
			if iceConfig == nil {
				iceConfig = &livekit.ICEConfig{}
			}
			if err = room.ResumeParticipant(participant, requestSource, responseSink,
				r.iceServersForRoom(protoRoom, iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TLS),
				pi.ReconnectReason); err != nil {
				logger.Warnw("could not resume participant", err, "participant", pi.Identity)
				return err
			}
			r.telemetry.ParticipantResumed(ctx, room.ToProto(), participant.ToProto(), livekit.NodeID(r.currentNode.Id), pi.ReconnectReason)
			go r.rtcSessionWorker(room, participant, requestSource)
			return nil
		} else {
			participant.GetLogger().Infow("removing duplicate participant")
			// we need to clean up the existing participant, so a new one can join
			room.RemoveParticipant(participant.Identity(), participant.ID(), types.ParticipantCloseReasonDuplicateIdentity)
		}
	} else if pi.Reconnect {
		// send leave request if participant is trying to reconnect without keep subscribe state
		// but missing from the room
		_ = responseSink.WriteMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{
				Leave: &livekit.LeaveRequest{
					CanReconnect: true,
					Reason:       livekit.DisconnectReason_STATE_MISMATCH,
				},
			},
		})
		return errors.New("could not restart participant")
	}

	logger.Debugw("starting RTC session",
		"room", roomName,
		"nodeID", r.currentNode.Id,
		"participant", pi.Identity,
		"sdk", pi.Client.Sdk,
		"sdkVersion", pi.Client.Version,
		"protocol", pi.Client.Protocol,
		"reconnect", pi.Reconnect,
		"reconnectReason", pi.ReconnectReason,
		"adaptiveStream", pi.AdaptiveStream,
	)

	clientConf := r.clientConfManager.GetConfiguration(pi.Client)

	pv := types.ProtocolVersion(pi.Client.Protocol)
	rtcConf := *r.rtcConfig
	rtcConf.SetBufferFactory(room.GetBufferFactory())
	sid := livekit.ParticipantID(utils.NewGuid(utils.ParticipantPrefix))
	pLogger := rtc.LoggerWithParticipant(room.Logger, pi.Identity, sid, false)
	// default allow forceTCP
	allowFallback := true
	if r.config.RTC.AllowTCPFallback != nil {
		allowFallback = *r.config.RTC.AllowTCPFallback
	}
	// default do not force full reconnect on a publication error
	reconnectOnPublicationError := false
	if r.config.RTC.ReconnectOnPublicationError != nil {
		reconnectOnPublicationError = *r.config.RTC.ReconnectOnPublicationError
	}
	// default do not force full reconnect on a subscription error
	reconnectOnSubscriptionError := false
	if r.config.RTC.ReconnectOnSubscriptionError != nil {
		reconnectOnSubscriptionError = *r.config.RTC.ReconnectOnSubscriptionError
	}
	participant, err = rtc.NewParticipant(rtc.ParticipantParams{
		Identity:                pi.Identity,
		Name:                    pi.Name,
		SID:                     sid,
		Config:                  &rtcConf,
		Sink:                    responseSink,
		AudioConfig:             r.config.Audio,
		VideoConfig:             r.config.Video,
		ProtocolVersion:         pv,
		Telemetry:               r.telemetry,
		PLIThrottleConfig:       r.config.RTC.PLIThrottle,
		CongestionControlConfig: r.config.RTC.CongestionControl,
		EnabledCodecs:           protoRoom.EnabledCodecs,
		Grants:                  pi.Grants,
		Logger:                  pLogger,
		ClientConf:              clientConf,
		ClientInfo:              rtc.ClientInfo{ClientInfo: pi.Client},
		Region:                  pi.Region,
		AdaptiveStream:          pi.AdaptiveStream,
		AllowTCPFallback:        allowFallback,
		TURNSEnabled:            r.config.IsTURNSEnabled(),
		GetParticipantInfo: func(pID livekit.ParticipantID) *livekit.ParticipantInfo {
			if p := room.GetParticipantByID(pID); p != nil {
				return p.ToProto()
			}
			return nil
		},
		ReconnectOnPublicationError:  reconnectOnPublicationError,
		ReconnectOnSubscriptionError: reconnectOnSubscriptionError,
		VersionGenerator:             r.versionGenerator,
		TrackResolver:                room.ResolveMediaTrackForSubscriber,
	})
	if err != nil {
		return err
	}
	iceConfig := r.setIceConfig(participant)

	// join room
	opts := rtc.ParticipantOptions{
		AutoSubscribe: pi.AutoSubscribe,
	}
	if err = room.Join(participant, requestSource, &opts, r.iceServersForRoom(protoRoom, iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TLS)); err != nil {
		pLogger.Errorw("could not join room", err)
		_ = participant.Close(true, types.ParticipantCloseReasonJoinFailed)
		return err
	}
	if err = r.roomStore.StoreParticipant(ctx, roomName, participant.ToProto()); err != nil {
		pLogger.Errorw("could not store participant", err)
	}

	persistRoomForParticipantCount := func(proto *livekit.Room) {
		if !participant.Hidden() {
			err = r.roomStore.StoreRoom(ctx, proto, room.Internal())
			if err != nil {
				logger.Errorw("could not store room", err)
			}
		}
	}

	// update room store with new numParticipants
	persistRoomForParticipantCount(room.ToProto())

	clientMeta := &livekit.AnalyticsClientMeta{Region: r.currentNode.Region, Node: r.currentNode.Id}
	r.telemetry.ParticipantJoined(ctx, protoRoom, participant.ToProto(), pi.Client, clientMeta, true)
	participant.OnClose(func(p types.LocalParticipant, disallowedSubscriptions map[livekit.TrackID]livekit.ParticipantID) {
		if err := r.roomStore.DeleteParticipant(ctx, roomName, p.Identity()); err != nil {
			pLogger.Errorw("could not delete participant", err)
		}

		// update room store with new numParticipants
		proto := room.ToProto()
		persistRoomForParticipantCount(proto)
		r.telemetry.ParticipantLeft(ctx, proto, p.ToProto(), true)

		room.RemoveDisallowedSubscriptions(p, disallowedSubscriptions)
	})
	participant.OnClaimsChanged(func(participant types.LocalParticipant) {
		pLogger.Debugw("refreshing client token after claims change")
		if err := r.refreshToken(participant); err != nil {
			logger.Errorw("could not refresh token", err)
		}
	})
	participant.OnICEConfigChanged(func(participant types.LocalParticipant, iceConfig *livekit.ICEConfig) {
		r.lock.Lock()
		r.iceConfigCache[participant.Identity()] = &iceConfigCacheEntry{
			iceConfig:  iceConfig,
			modifiedAt: time.Now(),
		}
		r.lock.Unlock()
	})

	go r.rtcSessionWorker(room, participant, requestSource)
	return nil
}

// create the actual room object, to be used on RTC node
func (r *RoomManager) getOrCreateRoom(ctx context.Context, roomName livekit.RoomName) (*rtc.Room, error) {
	r.lock.RLock()
	lastSeenRoom := r.rooms[roomName]
	r.lock.RUnlock()

	if lastSeenRoom != nil && lastSeenRoom.Hold() {
		return lastSeenRoom, nil
	}

	// create new room, get details first
	ri, internal, err := r.roomStore.LoadRoom(ctx, roomName, true)
	if err != nil {
		return nil, err
	}

	r.lock.Lock()

	currentRoom := r.rooms[roomName]
	for currentRoom != lastSeenRoom {
		r.lock.Unlock()
		if currentRoom != nil && currentRoom.Hold() {
			return currentRoom, nil
		}

		lastSeenRoom = currentRoom
		r.lock.Lock()
		currentRoom = r.rooms[roomName]
	}

	// construct ice servers
	newRoom := rtc.NewRoom(ri, internal, *r.rtcConfig, &r.config.Audio, r.serverInfo, r.telemetry, r.egressLauncher)

	newRoom.OnClose(func() {
		roomInfo := newRoom.ToProto()
		r.telemetry.RoomEnded(ctx, roomInfo)
		prometheus.RoomEnded(time.Unix(roomInfo.CreationTime, 0))
		if err := r.DeleteRoom(ctx, roomName); err != nil {
			newRoom.Logger.Errorw("could not delete room", err)
		}

		newRoom.Logger.Infow("room closed")
	})

	newRoom.OnMetadataUpdate(func(metadata string) {
		if err := r.roomStore.StoreRoom(ctx, newRoom.ToProto(), newRoom.Internal()); err != nil {
			newRoom.Logger.Errorw("could not handle metadata update", err)
		}
	})

	newRoom.OnParticipantChanged(func(p types.LocalParticipant) {
		if !p.IsDisconnected() {
			if err := r.roomStore.StoreParticipant(ctx, roomName, p.ToProto()); err != nil {
				newRoom.Logger.Errorw("could not handle participant change", err)
			}
		}
	})

	r.rooms[roomName] = newRoom

	r.lock.Unlock()

	newRoom.Hold()

	r.telemetry.RoomStarted(ctx, newRoom.ToProto())
	prometheus.RoomStarted()

	return newRoom, nil
}

// manages an RTC session for a participant, runs on the RTC node
func (r *RoomManager) rtcSessionWorker(room *rtc.Room, participant types.LocalParticipant, requestSource routing.MessageSource) {
	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.GetLogger(), room.Name(), room.ID()),
		participant.Identity(),
		participant.ID(),
		false,
	)
	defer func() {
		pLogger.Debugw("RTC session finishing")
		requestSource.Close()
	}()

	defer func() {
		if r := rtc.Recover(pLogger); r != nil {
			os.Exit(1)
		}
	}()

	// send first refresh for cases when client token is close to expiring
	_ = r.refreshToken(participant)
	tokenTicker := time.NewTicker(tokenRefreshInterval)
	defer tokenTicker.Stop()
	stateCheckTicker := time.NewTicker(time.Millisecond * 50)
	defer stateCheckTicker.Stop()
	for {
		select {
		case <-stateCheckTicker.C:
			// periodic check to ensure participant didn't become disconnected
			if participant.IsDisconnected() {
				return
			}
		case <-tokenTicker.C:
			// refresh token with the first API Key/secret pair
			if err := r.refreshToken(participant); err != nil {
				pLogger.Errorw("could not refresh token", err)
			}
		case obj := <-requestSource.ReadChan():
			// In single node mode, the request source is directly tied to the signal message channel
			// this means ICE restart isn't possible in single node mode
			if obj == nil {
				if room.GetParticipantRequestSource(participant.Identity()) == requestSource {
					participant.SetSignalSourceValid(false)
				}
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
func (r *RoomManager) handleRTCMessage(ctx context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) {
	r.lock.RLock()
	room := r.rooms[roomName]
	r.lock.RUnlock()

	if room == nil {
		if _, ok := msg.Message.(*livekit.RTCNodeMessage_DeleteRoom); ok {
			// special case of a non-RTC room e.g. room created but no participants joined
			logger.Debugw("Deleting non-rtc room, loading from roomstore")
			err := r.roomStore.DeleteRoom(ctx, roomName)
			if err != nil {
				logger.Debugw("Error deleting non-rtc room", "err", err)
			}
			return
		} else {
			logger.Warnw("Could not find room", nil, "room", roomName)
			return
		}
	}

	participant := room.GetParticipant(identity)
	var sid livekit.ParticipantID
	if participant != nil {
		sid = participant.ID()
	}
	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.GetLogger(), roomName, room.ID()),
		identity,
		sid,
		false,
	)

	switch rm := msg.Message.(type) {
	case *livekit.RTCNodeMessage_RemoveParticipant:
		if participant == nil {
			return
		}
		pLogger.Infow("removing participant")
		// remove participant by identity, any SID
		room.RemoveParticipant(identity, "", types.ParticipantCloseReasonServiceRequestRemoveParticipant)
	case *livekit.RTCNodeMessage_MuteTrack:
		if participant == nil {
			return
		}
		pLogger.Debugw("setting track muted",
			"trackID", rm.MuteTrack.TrackSid, "muted", rm.MuteTrack.Muted)
		if !rm.MuteTrack.Muted && !r.config.Room.EnableRemoteUnmute {
			pLogger.Errorw("cannot unmute track, remote unmute is disabled", nil)
			return
		}
		participant.SetTrackMuted(livekit.TrackID(rm.MuteTrack.TrackSid), rm.MuteTrack.Muted, true)
	case *livekit.RTCNodeMessage_UpdateParticipant:
		if participant == nil {
			return
		}
		pLogger.Debugw("updating participant", "metadata", rm.UpdateParticipant.Metadata,
			"permission", rm.UpdateParticipant.Permission)
		if rm.UpdateParticipant.Name != "" {
			participant.SetName(rm.UpdateParticipant.Name)
		}
		if rm.UpdateParticipant.Metadata != "" {
			participant.SetMetadata(rm.UpdateParticipant.Metadata)
		}
		if rm.UpdateParticipant.Permission != nil {
			participant.SetPermission(rm.UpdateParticipant.Permission)
		}
	case *livekit.RTCNodeMessage_DeleteRoom:
		room.Logger.Infow("deleting room")
		for _, p := range room.GetParticipants() {
			_ = p.Close(true, types.ParticipantCloseReasonServiceRequestDeleteRoom)
		}
		room.Close()
	case *livekit.RTCNodeMessage_UpdateSubscriptions:
		if participant == nil {
			return
		}
		pLogger.Debugw("updating participant subscriptions")
		room.UpdateSubscriptions(
			participant,
			livekit.StringsAsTrackIDs(rm.UpdateSubscriptions.TrackSids),
			rm.UpdateSubscriptions.ParticipantTracks,
			rm.UpdateSubscriptions.Subscribe,
		)
	case *livekit.RTCNodeMessage_SendData:
		pLogger.Debugw("api send data", "size", len(rm.SendData.Data))
		up := &livekit.UserPacket{
			Payload:         rm.SendData.Data,
			DestinationSids: rm.SendData.DestinationSids,
			Topic:           rm.SendData.Topic,
		}
		room.SendDataPacket(up, rm.SendData.Kind)
	case *livekit.RTCNodeMessage_UpdateRoomMetadata:
		pLogger.Debugw("updating room")
		room.SetMetadata(rm.UpdateRoomMetadata.Metadata)
	}
}

func (r *RoomManager) iceServersForRoom(ri *livekit.Room, tlsOnly bool) []*livekit.ICEServer {
	var iceServers []*livekit.ICEServer
	rtcConf := r.config.RTC

	if tlsOnly && r.config.TURN.TLSPort == 0 {
		logger.Warnw("tls only enabled but no turn tls config", nil)
		tlsOnly = false
	}

	hasSTUN := false
	if r.config.TURN.Enabled {
		var urls []string
		if r.config.TURN.UDPPort > 0 && !tlsOnly {
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

	if len(rtcConf.TURNServers) > 0 {
		hasSTUN = true
		for _, s := range r.config.RTC.TURNServers {
			scheme := "turn"
			transport := "tcp"
			if s.Protocol == "tls" {
				scheme = "turns"
			} else if s.Protocol == "udp" {
				transport = "udp"
			}
			is := &livekit.ICEServer{
				Urls: []string{
					fmt.Sprintf("%s:%s:%d?transport=%s", scheme, s.Host, s.Port, transport),
				},
				Username:   s.Username,
				Credential: s.Credential,
			}
			iceServers = append(iceServers, is)
		}
	}

	if len(rtcConf.STUNServers) > 0 {
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

func (r *RoomManager) setIceConfig(participant types.LocalParticipant) *livekit.ICEConfig {
	iceConfig := r.getIceConfig(participant)
	if iceConfig == nil {
		return &livekit.ICEConfig{}
	}
	participant.SetICEConfig(iceConfig)
	return iceConfig
}

func (r *RoomManager) getIceConfig(participant types.LocalParticipant) *livekit.ICEConfig {
	r.lock.Lock()
	defer r.lock.Unlock()
	iceConfigCacheEntry, ok := r.iceConfigCache[participant.Identity()]
	if !ok || time.Since(iceConfigCacheEntry.modifiedAt) > iceConfigTTL {
		delete(r.iceConfigCache, participant.Identity())
		return nil
	}
	return iceConfigCacheEntry.iceConfig
}

// ------------------------------------

func iceServerForStunServers(servers []string) *livekit.ICEServer {
	iceServer := &livekit.ICEServer{}
	for _, stunServer := range servers {
		iceServer.Urls = append(iceServer.Urls, fmt.Sprintf("stun:%s", stunServer))
	}
	return iceServer
}
