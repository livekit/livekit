// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/exp/maps"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/sfu"
	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/protocol/utils/must"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/middleware"

	"github.com/livekit/livekit-server/pkg/clientconfiguration"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/version"
)

const (
	tokenRefreshInterval = 5 * time.Minute
	tokenDefaultTTL      = 10 * time.Minute
)

type iceConfigCacheKey struct {
	roomName            livekit.RoomName
	participantIdentity livekit.ParticipantIdentity
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
	roomAllocator     RoomAllocator
	roomManagerServer rpc.TypedRoomManagerServer
	whipServer        rpc.WHIPServer[livekit.NodeID]
	roomStore         ObjectStore
	telemetry         telemetry.TelemetryService
	clientConfManager clientconfiguration.ClientConfigurationManager
	agentClient       agent.Client
	agentStore        AgentStore
	egressLauncher    rtc.EgressLauncher
	versionGenerator  utils.TimedVersionGenerator
	turnAuthHandler   *TURNAuthHandler
	bus               psrpc.MessageBus

	rooms map[livekit.RoomName]*rtc.Room

	roomServers                  utils.MultitonService[rpc.RoomTopic]
	agentDispatchServers         utils.MultitonService[rpc.RoomTopic]
	participantServers           utils.MultitonService[rpc.ParticipantTopic]
	httpSignalParticipantServers utils.MultitonService[rpc.ParticipantTopic]
	whipParticipantServers       utils.MultitonService[rpc.ParticipantTopic]

	iceConfigCache *sutils.IceConfigCache[iceConfigCacheKey]

	forwardStats *sfu.ForwardStats

	rpc.UnimplementedParticipantServer
	rpc.UnimplementedRoomServer
	rpc.UnimplementedRoomManagerServer
}

func NewLocalRoomManager(
	conf *config.Config,
	roomStore ObjectStore,
	currentNode routing.LocalNode,
	router routing.Router,
	roomAllocator RoomAllocator,
	telemetry telemetry.TelemetryService,
	agentClient agent.Client,
	agentStore AgentStore,
	egressLauncher rtc.EgressLauncher,
	versionGenerator utils.TimedVersionGenerator,
	turnAuthHandler *TURNAuthHandler,
	bus psrpc.MessageBus,
	forwardStats *sfu.ForwardStats,
) (*RoomManager, error) {
	rtcConf, err := rtc.NewWebRTCConfig(conf)
	if err != nil {
		return nil, err
	}

	r := &RoomManager{
		config:            conf,
		rtcConfig:         rtcConf,
		currentNode:       currentNode,
		router:            router,
		roomAllocator:     roomAllocator,
		roomStore:         roomStore,
		telemetry:         telemetry,
		clientConfManager: clientconfiguration.NewStaticClientConfigurationManager(clientconfiguration.StaticConfigurations),
		egressLauncher:    egressLauncher,
		agentClient:       agentClient,
		agentStore:        agentStore,
		versionGenerator:  versionGenerator,
		turnAuthHandler:   turnAuthHandler,
		bus:               bus,
		forwardStats:      forwardStats,

		rooms: make(map[livekit.RoomName]*rtc.Room),

		iceConfigCache: sutils.NewIceConfigCache[iceConfigCacheKey](0),

		serverInfo: &livekit.ServerInfo{
			Edition:       livekit.ServerInfo_Standard,
			Version:       version.Version,
			Protocol:      types.CurrentProtocol,
			AgentProtocol: agent.CurrentProtocol,
			Region:        conf.Region,
			NodeId:        string(currentNode.NodeID()),
		},
	}

	r.roomManagerServer, err = rpc.NewTypedRoomManagerServer(r, bus, rpc.WithServerLogger(logger.GetLogger()), middleware.WithServerMetrics(rpc.PSRPCMetricsObserver{}), psrpc.WithServerChannelSize(conf.PSRPC.BufferSize))
	if err != nil {
		return nil, err
	}
	if err := r.roomManagerServer.RegisterAllNodeTopics(currentNode.NodeID()); err != nil {
		return nil, err
	}

	whipService, err := newWhipService(r)
	if err != nil {
		return nil, err
	}
	r.whipServer, err = rpc.NewWHIPServer[livekit.NodeID](whipService, bus, rpc.WithDefaultServerOptions(conf.PSRPC, logger.GetLogger()))
	if err != nil {
		return nil, err
	}
	if err := r.whipServer.RegisterAllCommonTopics(currentNode.NodeID()); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *RoomManager) GetRoom(_ context.Context, roomName livekit.RoomName) *rtc.Room {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.rooms[roomName]
}

// deleteRoom completely deletes all room information, including active sessions, room store, and routing info
func (r *RoomManager) deleteRoom(ctx context.Context, roomName livekit.RoomName) error {
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

func (r *RoomManager) CloseIdleRooms() {
	r.lock.RLock()
	rooms := maps.Values(r.rooms)
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
	rooms := maps.Values(r.rooms)
	r.lock.RUnlock()

	for _, room := range rooms {
		room.Close(types.ParticipantCloseReasonRoomManagerStop)
	}

	r.roomManagerServer.Kill()
	r.whipServer.Kill()
	r.roomServers.Kill()
	r.agentDispatchServers.Kill()
	r.participantServers.Kill()
	r.httpSignalParticipantServers.Kill()
	r.whipParticipantServers.Kill()

	if r.rtcConfig != nil {
		if r.rtcConfig.UDPMux != nil {
			_ = r.rtcConfig.UDPMux.Close()
		}
		if r.rtcConfig.TCPMuxListener != nil {
			_ = r.rtcConfig.TCPMuxListener.Close()
		}
	}

	r.iceConfigCache.Stop()

	if r.forwardStats != nil {
		r.forwardStats.Stop()
	}
}

func (r *RoomManager) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	room, err := r.getOrCreateRoom(ctx, req)
	if err != nil {
		return nil, err
	}
	defer room.Release()

	return room.ToProto(), nil
}

// StartSession starts WebRTC session when a new participant is connected, takes place on RTC node
func (r *RoomManager) StartSession(
	ctx context.Context,
	pi routing.ParticipantInit,
	requestSource routing.MessageSource,
	responseSink routing.MessageSink,
	useOneShotSignallingMode bool,
) error {
	sessionStartTime := time.Now()

	createRoom := pi.CreateRoom
	room, err := r.getOrCreateRoom(ctx, createRoom)
	if err != nil {
		return err
	}
	defer room.Release()

	protoRoom, roomInternal := room.ToProto(), room.Internal()

	// only create the room, but don't start a participant session
	if pi.Identity == "" {
		return nil
	}

	// should not error out, error is logged in iceServersForParticipant even if it fails
	// since this is used for TURN server credentials, we don't want to fail the request even if there's no TURN for the session
	apiKey, _, _ := r.getFirstKeyPair()

	participant := room.GetParticipant(pi.Identity)
	if participant != nil {
		// When reconnecting, it means WS has interrupted but underlying peer connection is still ok in this state,
		// we'll keep the participant SID, and just swap the sink for the underlying connection
		if pi.Reconnect {
			if participant.IsClosed() {
				// Send leave request if participant is closed, i. e. handle the case of client trying to resume crossing wires with
				// server closing the participant due to some irrecoverable condition. Such a condition would have triggered
				// a full reconnect when that condition occurred.
				//
				// It is possible that the client did not get that send request. So, send it again.
				logger.Infow("cannot restart a closed participant",
					"room", room.Name(),
					"nodeID", r.currentNode.NodeID(),
					"participant", pi.Identity,
					"reason", pi.ReconnectReason,
				)

				var leave *livekit.LeaveRequest
				pv := types.ProtocolVersion(pi.Client.Protocol)
				if pv.SupportsRegionsInLeaveRequest() {
					leave = &livekit.LeaveRequest{
						Reason: livekit.DisconnectReason_STATE_MISMATCH,
						Action: livekit.LeaveRequest_RECONNECT,
					}
				} else {
					leave = &livekit.LeaveRequest{
						CanReconnect: true,
						Reason:       livekit.DisconnectReason_STATE_MISMATCH,
					}
				}
				_ = responseSink.WriteMessage(&livekit.SignalResponse{
					Message: &livekit.SignalResponse_Leave{
						Leave: leave,
					},
				})
				return errors.New("could not restart closed participant")
			}

			participant.GetLogger().Infow(
				"resuming RTC session",
				"nodeID", r.currentNode.NodeID(),
				"participantInit", &pi,
				"numParticipants", room.GetParticipantCount(),
			)
			iceConfig := r.getIceConfig(room.Name(), participant)
			if err = room.ResumeParticipant(
				participant,
				requestSource,
				responseSink,
				iceConfig,
				r.iceServersForParticipant(
					apiKey,
					participant,
					iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TLS,
				),
				pi.ReconnectReason,
			); err != nil {
				participant.GetLogger().Warnw("could not resume participant", err)
				return err
			}
			r.telemetry.ParticipantResumed(ctx, room.ToProto(), participant.ToProto(), r.currentNode.NodeID(), pi.ReconnectReason)

			go room.HandleSyncState(participant, pi.SyncState)

			go r.rtcSessionWorker(room, participant, requestSource)
			return nil
		}

		// we need to clean up the existing participant, so a new one can join
		participant.GetLogger().Infow("removing duplicate participant")
		room.RemoveParticipant(participant.Identity(), participant.ID(), types.ParticipantCloseReasonDuplicateIdentity)
	} else if pi.Reconnect {
		// send leave request if participant is trying to reconnect without keep subscribe state
		// but missing from the room
		var leave *livekit.LeaveRequest
		pv := types.ProtocolVersion(pi.Client.Protocol)
		if pv.SupportsRegionsInLeaveRequest() {
			leave = &livekit.LeaveRequest{
				Reason: livekit.DisconnectReason_STATE_MISMATCH,
				Action: livekit.LeaveRequest_RECONNECT,
			}
		} else {
			leave = &livekit.LeaveRequest{
				CanReconnect: true,
				Reason:       livekit.DisconnectReason_STATE_MISMATCH,
			}
		}
		_ = responseSink.WriteMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{
				Leave: leave,
			},
		})
		return errors.New("could not restart participant")
	}

	sid := livekit.ParticipantID(guid.New(utils.ParticipantPrefix))
	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.GetLogger(), room.Name(), room.ID()),
		pi.Identity,
		sid,
		false,
	)
	pLogger.Infow(
		"starting RTC session",
		"room", room.Name(),
		"nodeID", r.currentNode.NodeID(),
		"numParticipants", room.GetParticipantCount(),
		"participantInit", &pi,
	)

	clientConf := r.clientConfManager.GetConfiguration(pi.Client)

	pv := types.ProtocolVersion(pi.Client.Protocol)
	rtcConf := *r.rtcConfig
	rtcConf.SetBufferFactory(room.GetBufferFactory())
	if pi.DisableICELite {
		rtcConf.SettingEngine.SetLite(false)
	}
	rtcConf.UpdatePublisherConfig(pi.UseSinglePeerConnection)

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

	// default do not force full reconnect on a data channel error
	reconnectOnDataChannelError := false
	if r.config.RTC.ReconnectOnDataChannelError != nil {
		reconnectOnDataChannelError = *r.config.RTC.ReconnectOnDataChannelError
	}

	subscriberAllowPause := r.config.RTC.CongestionControl.AllowPause
	if pi.SubscriberAllowPause != nil {
		subscriberAllowPause = *pi.SubscriberAllowPause
	}

	participant, err = rtc.NewParticipant(rtc.ParticipantParams{
		Identity:                pi.Identity,
		Name:                    pi.Name,
		SID:                     sid,
		Config:                  &rtcConf,
		Sink:                    responseSink,
		AudioConfig:             r.config.Audio,
		VideoConfig:             r.config.Video,
		LimitConfig:             r.config.Limit,
		ProtocolVersion:         pv,
		SessionStartTime:        sessionStartTime,
		Telemetry:               r.telemetry,
		Trailer:                 room.Trailer(),
		PLIThrottleConfig:       r.config.RTC.PLIThrottle,
		CongestionControlConfig: r.config.RTC.CongestionControl,
		PublishEnabledCodecs:    protoRoom.EnabledCodecs,
		SubscribeEnabledCodecs:  protoRoom.EnabledCodecs,
		Grants:                  pi.Grants,
		Reconnect:               pi.Reconnect,
		Logger:                  pLogger,
		Reporter:                roomobs.NewNoopParticipantSessionReporter(),
		ClientConf:              clientConf,
		ClientInfo:              rtc.ClientInfo{ClientInfo: pi.Client},
		Region:                  pi.Region,
		AdaptiveStream:          pi.AdaptiveStream,
		AllowTCPFallback:        allowFallback,
		TURNSEnabled:            r.config.IsTURNSEnabled(),
		ParticipantHelper: &roomManagerParticipantHelper{
			room:                     room,
			codecRegressionThreshold: r.config.Video.CodecRegressionThreshold,
		},
		ReconnectOnPublicationError:  reconnectOnPublicationError,
		ReconnectOnSubscriptionError: reconnectOnSubscriptionError,
		ReconnectOnDataChannelError:  reconnectOnDataChannelError,
		VersionGenerator:             r.versionGenerator,
		SubscriberAllowPause:         subscriberAllowPause,
		SubscriptionLimitAudio:       r.config.Limit.SubscriptionLimitAudio,
		SubscriptionLimitVideo:       r.config.Limit.SubscriptionLimitVideo,
		PlayoutDelay:                 roomInternal.GetPlayoutDelay(),
		SyncStreams:                  roomInternal.GetSyncStreams(),
		ForwardStats:                 r.forwardStats,
		MetricConfig:                 r.config.Metric,
		UseOneShotSignallingMode:     useOneShotSignallingMode,
		DataChannelMaxBufferedAmount: r.config.RTC.DataChannelMaxBufferedAmount,
		DatachannelSlowThreshold:     r.config.RTC.DatachannelSlowThreshold,
		FireOnTrackBySdp:             true,
		UseSinglePeerConnection:      pi.UseSinglePeerConnection,
	})
	if err != nil {
		return err
	}
	iceConfig := r.setIceConfig(room.Name(), participant)

	// join room
	opts := rtc.ParticipantOptions{
		AutoSubscribe: pi.AutoSubscribe,
	}
	iceServers := r.iceServersForParticipant(apiKey, participant, iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TLS)
	if err = room.Join(participant, requestSource, &opts, iceServers); err != nil {
		pLogger.Errorw("could not join room", err)
		_ = participant.Close(true, types.ParticipantCloseReasonJoinFailed, false)
		return err
	}

	var participantServerClosers utils.Closers
	participantTopic := rpc.FormatParticipantTopic(room.Name(), participant.Identity())
	participantServer := must.Get(rpc.NewTypedParticipantServer(r, r.bus))
	participantServerClosers = append(participantServerClosers, utils.CloseFunc(r.participantServers.Replace(participantTopic, participantServer)))
	if err := participantServer.RegisterAllParticipantTopics(participantTopic); err != nil {
		participantServerClosers.Close()
		pLogger.Errorw("could not join register participant topic", err)
		_ = participant.Close(true, types.ParticipantCloseReasonMessageBusFailed, false)
		return err
	}

	if useOneShotSignallingMode {
		whipParticipantServer := must.Get(rpc.NewTypedWHIPParticipantServer(whipParticipantService{r}, r.bus))
		participantServerClosers = append(participantServerClosers, utils.CloseFunc(r.whipParticipantServers.Replace(participantTopic, whipParticipantServer)))
		if err := whipParticipantServer.RegisterAllCommonTopics(participantTopic); err != nil {
			participantServerClosers.Close()
			pLogger.Errorw("could not join register participant topic for rtc rest participant server", err)
			_ = participant.Close(true, types.ParticipantCloseReasonMessageBusFailed, false)
			return err
		}
	}

	if err = r.roomStore.StoreParticipant(ctx, room.Name(), participant.ToProto()); err != nil {
		pLogger.Errorw("could not store participant", err)
	}

	persistRoomForParticipantCount := func(proto *livekit.Room) {
		if !participant.Hidden() && !room.IsClosed() {
			err = r.roomStore.StoreRoom(ctx, proto, room.Internal())
			if err != nil {
				logger.Errorw("could not store room", err)
			}
		}
	}

	// update room store with new numParticipants
	persistRoomForParticipantCount(room.ToProto())

	clientMeta := &livekit.AnalyticsClientMeta{Region: r.currentNode.Region(), Node: string(r.currentNode.NodeID())}
	r.telemetry.ParticipantJoined(ctx, protoRoom, participant.ToProto(), pi.Client, clientMeta, true, participant.TelemetryGuard())
	participant.AddOnClose(types.ParticipantCloseKeyNormal, func(p types.LocalParticipant) {
		participantServerClosers.Close()

		if err := r.roomStore.DeleteParticipant(ctx, room.Name(), p.Identity()); err != nil {
			pLogger.Errorw("could not delete participant", err)
		}

		// update room store with new numParticipants
		proto := room.ToProto()
		persistRoomForParticipantCount(proto)
		r.telemetry.ParticipantLeft(ctx, proto, p.ToProto(), true, participant.TelemetryGuard())
	})
	participant.OnClaimsChanged(func(participant types.LocalParticipant) {
		pLogger.Debugw("refreshing client token after claims change")
		if err := r.refreshToken(participant); err != nil {
			pLogger.Errorw("could not refresh token", err)
		}
	})
	participant.OnICEConfigChanged(func(participant types.LocalParticipant, iceConfig *livekit.ICEConfig) {
		r.iceConfigCache.Put(iceConfigCacheKey{room.Name(), participant.Identity()}, iceConfig)
	})

	for _, addTrackRequest := range pi.AddTrackRequests {
		participant.AddTrack(addTrackRequest)
	}
	if pi.PublisherOffer != nil {
		participant.HandleOffer(pi.PublisherOffer)
	}

	go r.rtcSessionWorker(room, participant, requestSource)
	return nil
}

// create the actual room object, to be used on RTC node
func (r *RoomManager) getOrCreateRoom(ctx context.Context, createRoom *livekit.CreateRoomRequest) (*rtc.Room, error) {
	roomName := livekit.RoomName(createRoom.Name)

	r.lock.RLock()
	lastSeenRoom := r.rooms[roomName]
	r.lock.RUnlock()

	if lastSeenRoom != nil && lastSeenRoom.Hold() {
		return lastSeenRoom, nil
	}

	// create new room, get details first
	ri, internal, created, err := r.roomAllocator.CreateRoom(ctx, createRoom, true)
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
	newRoom := rtc.NewRoom(ri, internal, *r.rtcConfig, r.config.Room, &r.config.Audio, r.serverInfo, r.telemetry, r.agentClient, r.agentStore, r.egressLauncher)

	roomTopic := rpc.FormatRoomTopic(roomName)
	roomServer := must.Get(rpc.NewTypedRoomServer(r, r.bus))
	killRoomServer := r.roomServers.Replace(roomTopic, roomServer)
	if err := roomServer.RegisterAllRoomTopics(roomTopic); err != nil {
		killRoomServer()
		r.lock.Unlock()
		return nil, err
	}
	agentDispatchServer := must.Get(rpc.NewTypedAgentDispatchInternalServer(r, r.bus))
	killDispServer := r.agentDispatchServers.Replace(roomTopic, agentDispatchServer)
	if err := agentDispatchServer.RegisterAllRoomTopics(roomTopic); err != nil {
		killRoomServer()
		killDispServer()
		r.lock.Unlock()
		return nil, err
	}

	newRoom.OnClose(func() {
		killRoomServer()
		killDispServer()

		roomInfo := newRoom.ToProto()
		r.telemetry.RoomEnded(ctx, roomInfo)
		prometheus.RoomEnded(time.Unix(roomInfo.CreationTime, 0))
		if err := r.deleteRoom(ctx, roomName); err != nil {
			newRoom.Logger().Errorw("could not delete room", err)
		}

		newRoom.Logger().Infow("room closed")
	})

	newRoom.OnRoomUpdated(func() {
		if err := r.roomStore.StoreRoom(ctx, newRoom.ToProto(), newRoom.Internal()); err != nil {
			newRoom.Logger().Errorw("could not handle metadata update", err)
		}
	})

	newRoom.OnParticipantChanged(func(p types.LocalParticipant) {
		if !p.IsDisconnected() {
			if err := r.roomStore.StoreParticipant(ctx, roomName, p.ToProto()); err != nil {
				newRoom.Logger().Errorw("could not handle participant change", err)
			}
		}
	})

	r.rooms[roomName] = newRoom

	r.lock.Unlock()

	newRoom.Hold()

	r.telemetry.RoomStarted(ctx, newRoom.ToProto())
	prometheus.RoomStarted()

	if created && createRoom.GetEgress().GetRoom() != nil {
		// ensure room name matches
		createRoom.Egress.Room.RoomName = createRoom.Name
		_, err = r.egressLauncher.StartEgress(ctx, &rpc.StartEgressRequest{
			Request: &rpc.StartEgressRequest_RoomComposite{
				RoomComposite: createRoom.Egress.Room,
			},
			RoomId: ri.Sid,
		})
		if err != nil {
			newRoom.Release()
			return nil, err
		}
	}

	return newRoom, nil
}

// manages an RTC session for a participant, runs on the RTC node
func (r *RoomManager) rtcSessionWorker(room *rtc.Room, participant types.LocalParticipant, requestSource routing.MessageSource) {
	pLogger := participant.GetLogger()
	defer func() {
		pLogger.Debugw("RTC session finishing", "connID", requestSource.ConnectionID())
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
	for {
		select {
		case <-participant.Disconnected():
			return

		case <-tokenTicker.C:
			// refresh token with the first API Key/secret pair
			if err := r.refreshToken(participant); err != nil {
				pLogger.Errorw("could not refresh token", err, "connID", requestSource.ConnectionID())
			}

		case obj := <-requestSource.ReadChan():
			if obj == nil {
				if room.GetParticipantRequestSource(participant.Identity()) == requestSource {
					participant.HandleSignalSourceClose()
				}
				return
			}

			if err := participant.HandleSignalMessage(obj); err != nil {
				// more specific errors are already logged
				// treat errors returned as fatal
				return
			}
		}
	}
}

type participantReq interface {
	GetRoom() string
	GetIdentity() string
}

func (r *RoomManager) roomAndParticipantForReq(ctx context.Context, req participantReq) (*rtc.Room, types.LocalParticipant, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.GetRoom()))
	if room == nil {
		return nil, nil, ErrRoomNotFound
	}

	participant := room.GetParticipant(livekit.ParticipantIdentity(req.GetIdentity()))
	if participant == nil {
		return nil, nil, ErrParticipantNotFound
	}

	return room, participant, nil
}

func (r *RoomManager) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	room, participant, err := r.roomAndParticipantForReq(ctx, req)
	if err != nil {
		return nil, err
	}

	participant.GetLogger().Infow("removing participant")
	room.RemoveParticipant(livekit.ParticipantIdentity(req.Identity), "", types.ParticipantCloseReasonServiceRequestRemoveParticipant)
	return &livekit.RemoveParticipantResponse{}, nil
}

func (r *RoomManager) MutePublishedTrack(ctx context.Context, req *livekit.MuteRoomTrackRequest) (*livekit.MuteRoomTrackResponse, error) {
	_, participant, err := r.roomAndParticipantForReq(ctx, req)
	if err != nil {
		return nil, err
	}

	participant.GetLogger().Debugw("setting track muted",
		"trackID", req.TrackSid, "muted", req.Muted)
	if !req.Muted && !r.config.Room.EnableRemoteUnmute {
		participant.GetLogger().Errorw("cannot unmute track, remote unmute is disabled", nil)
		return nil, ErrRemoteUnmuteNoteEnabled
	}
	track := participant.SetTrackMuted(&livekit.MuteTrackRequest{
		Sid:   req.TrackSid,
		Muted: req.Muted,
	}, true)
	return &livekit.MuteRoomTrackResponse{Track: track}, nil
}

func (r *RoomManager) UpdateParticipant(ctx context.Context, req *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error) {
	_, participant, err := r.roomAndParticipantForReq(ctx, req)
	if err != nil {
		return nil, err
	}

	if err = participant.UpdateMetadata(&livekit.UpdateParticipantMetadata{
		Name:       req.Name,
		Metadata:   req.Metadata,
		Attributes: req.Attributes,
	}, true); err != nil {
		return nil, err
	}

	if req.Permission != nil {
		participant.GetLogger().Debugw(
			"updating participant permission",
			"permission", req.Permission,
		)

		participant.SetPermission(req.Permission)
	}

	return participant.ToProto(), nil
}

func (r *RoomManager) ForwardParticipant(ctx context.Context, req *livekit.ForwardParticipantRequest) (*livekit.ForwardParticipantResponse, error) {
	return nil, errors.New("not implemented")
}

func (r *RoomManager) MoveParticipant(ctx context.Context, req *livekit.MoveParticipantRequest) (*livekit.MoveParticipantResponse, error) {
	return nil, errors.New("not implemented")
}

func (r *RoomManager) PerformRpc(ctx context.Context, req *livekit.PerformRpcRequest) (*livekit.PerformRpcResponse, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.GetRoom()))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	participant := room.GetParticipant(livekit.ParticipantIdentity(req.GetDestinationIdentity()))
	if participant == nil {
		return nil, ErrParticipantNotFound
	}

	resultChan := make(chan string, 1)
	errorChan := make(chan error, 1)

	participant.PerformRpc(req, resultChan, errorChan)

	select {
	case result := <-resultChan:
		return &livekit.PerformRpcResponse{Payload: result}, nil
	case err := <-errorChan:
		return nil, err
	}
}

func (r *RoomManager) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		// special case of a non-RTC room e.g. room created but no participants joined
		logger.Debugw("Deleting non-rtc room, loading from roomstore")
		err := r.roomStore.DeleteRoom(ctx, livekit.RoomName(req.Room))
		if err != nil {
			logger.Debugw("Error deleting non-rtc room", "err", err)
			return nil, err
		}
	} else {
		room.Logger().Infow("deleting room")
		room.Close(types.ParticipantCloseReasonServiceRequestDeleteRoom)
	}
	return &livekit.DeleteRoomResponse{}, nil
}

func (r *RoomManager) UpdateSubscriptions(ctx context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error) {
	room, participant, err := r.roomAndParticipantForReq(ctx, req)
	if err != nil {
		return nil, err
	}

	participant.GetLogger().Debugw("updating participant subscriptions")
	room.UpdateSubscriptions(
		participant,
		livekit.StringsAsIDs[livekit.TrackID](req.TrackSids),
		req.ParticipantTracks,
		req.Subscribe,
	)
	return &livekit.UpdateSubscriptionsResponse{}, nil
}

func (r *RoomManager) SendData(ctx context.Context, req *livekit.SendDataRequest) (*livekit.SendDataResponse, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	room.Logger().Debugw("api send data", "size", len(req.Data))
	room.SendDataPacket(&livekit.DataPacket{
		Kind:                  req.Kind,
		DestinationIdentities: req.DestinationIdentities,
		Value: &livekit.DataPacket_User{
			User: &livekit.UserPacket{
				Payload:               req.Data,
				DestinationSids:       req.DestinationSids,
				DestinationIdentities: req.DestinationIdentities,
				Topic:                 req.Topic,
				Nonce:                 req.Nonce,
			},
		},
	}, req.Kind)
	return &livekit.SendDataResponse{}, nil
}

func (r *RoomManager) UpdateRoomMetadata(ctx context.Context, req *livekit.UpdateRoomMetadataRequest) (*livekit.Room, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	room.Logger().Debugw("updating room")
	done := room.SetMetadata(req.Metadata)
	// wait till the update is applied
	<-done
	return room.ToProto(), nil
}

func (r *RoomManager) ListDispatch(ctx context.Context, req *livekit.ListAgentDispatchRequest) (*livekit.ListAgentDispatchResponse, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	disp, err := room.GetAgentDispatches(req.DispatchId)
	if err != nil {
		return nil, err
	}

	ret := &livekit.ListAgentDispatchResponse{
		AgentDispatches: disp,
	}

	return ret, nil
}

func (r *RoomManager) CreateDispatch(ctx context.Context, req *livekit.AgentDispatch) (*livekit.AgentDispatch, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	disp, err := room.AddAgentDispatch(req)
	if err != nil {
		return nil, err
	}

	return disp, nil
}

func (r *RoomManager) DeleteDispatch(ctx context.Context, req *livekit.DeleteAgentDispatchRequest) (*livekit.AgentDispatch, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	disp, err := room.DeleteAgentDispatch(req.DispatchId)
	if err != nil {
		return nil, err
	}

	return disp, nil
}

func (r *RoomManager) iceServersForParticipant(apiKey string, participant types.LocalParticipant, tlsOnly bool) []*livekit.ICEServer {
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
			username := r.turnAuthHandler.CreateUsername(apiKey, participant.ID())
			password, err := r.turnAuthHandler.CreatePassword(apiKey, participant.ID())
			if err != nil {
				participant.GetLogger().Warnw("could not create turn password", err)
				hasSTUN = false
			} else {
				logger.Infow("created TURN password", "username", username, "password", password)
				iceServers = append(iceServers, &livekit.ICEServer{
					Urls:       urls,
					Username:   username,
					Credential: password,
				})
			}
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
		iceServers = append(iceServers, iceServerForStunServers(rtcconfig.DefaultStunServers))
	}
	return iceServers
}

func (r *RoomManager) refreshToken(participant types.LocalParticipant) error {
	key, secret, err := r.getFirstKeyPair()
	if err != nil {
		return err
	}

	grants := participant.ClaimGrants()
	token := auth.NewAccessToken(key, secret)
	token.SetName(grants.Name).
		SetIdentity(string(participant.Identity())).
		SetKind(grants.GetParticipantKind()).
		SetValidFor(tokenDefaultTTL).
		SetMetadata(grants.Metadata).
		SetAttributes(grants.Attributes).
		SetVideoGrant(grants.Video).
		SetRoomConfig(grants.GetRoomConfiguration()).
		SetRoomPreset(grants.RoomPreset)
	jwt, err := token.ToJWT()
	if err == nil {
		err = participant.SendRefreshToken(jwt)
	}
	if err != nil {
		return err
	}

	return nil
}

func (r *RoomManager) setIceConfig(roomName livekit.RoomName, participant types.LocalParticipant) *livekit.ICEConfig {
	iceConfig := r.getIceConfig(roomName, participant)
	participant.SetICEConfig(iceConfig)
	return iceConfig
}

func (r *RoomManager) getIceConfig(roomName livekit.RoomName, participant types.LocalParticipant) *livekit.ICEConfig {
	return r.iceConfigCache.Get(iceConfigCacheKey{roomName, participant.Identity()})
}

func (r *RoomManager) getFirstKeyPair() (string, string, error) {
	for key, secret := range r.config.Keys {
		return key, secret, nil
	}
	return "", "", errors.New("no API keys configured")
}

// ------------------------------------

func iceServerForStunServers(servers []string) *livekit.ICEServer {
	iceServer := &livekit.ICEServer{}
	for _, stunServer := range servers {
		iceServer.Urls = append(iceServer.Urls, fmt.Sprintf("stun:%s", stunServer))
	}
	return iceServer
}

// ------------------------------------

type roomManagerParticipantHelper struct {
	room                     *rtc.Room
	codecRegressionThreshold int
}

func (h *roomManagerParticipantHelper) GetParticipantInfo(pID livekit.ParticipantID) *livekit.ParticipantInfo {
	if p := h.room.GetParticipantByID(pID); p != nil {
		return p.ToProto()
	}
	return nil
}

func (h *roomManagerParticipantHelper) GetRegionSettings(ip string) *livekit.RegionSettings {
	return nil
}

func (h *roomManagerParticipantHelper) GetSubscriberForwarderState(lp types.LocalParticipant) (map[livekit.TrackID]*livekit.RTPForwarderState, error) {
	return nil, nil
}

func (h *roomManagerParticipantHelper) ResolveMediaTrack(lp types.LocalParticipant, trackID livekit.TrackID) types.MediaResolverResult {
	return h.room.ResolveMediaTrackForSubscriber(lp, trackID)
}

func (h *roomManagerParticipantHelper) ShouldRegressCodec() bool {
	return h.codecRegressionThreshold == 0 || h.room.GetParticipantCount() < h.codecRegressionThreshold
}

func (h *roomManagerParticipantHelper) GetCachedReliableDataMessage(seqs map[livekit.ParticipantID]uint32) []*types.DataMessageCache {
	return h.room.GetCachedReliableDataMessage(seqs)
}
