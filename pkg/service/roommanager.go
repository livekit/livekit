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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/exp/maps"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/version"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"

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

var affinityEpoch = time.Date(2000, 0, 0, 0, 0, 0, 0, time.UTC)

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
	agentClient       rtc.AgentClient
	egressLauncher    rtc.EgressLauncher
	versionGenerator  utils.TimedVersionGenerator
	turnAuthHandler   *TURNAuthHandler
	bus               psrpc.MessageBus

	rooms map[livekit.RoomName]*rtc.Room

	roomServers        utils.MultitonService[rpc.RoomTopic]
	participantServers utils.MultitonService[rpc.ParticipantTopic]

	iceConfigCache map[livekit.ParticipantIdentity]*iceConfigCacheEntry
}

func NewLocalRoomManager(
	conf *config.Config,
	roomStore ObjectStore,
	currentNode routing.LocalNode,
	router routing.Router,
	telemetry telemetry.TelemetryService,
	clientConfManager clientconfiguration.ClientConfigurationManager,
	agentClient rtc.AgentClient,
	egressLauncher rtc.EgressLauncher,
	versionGenerator utils.TimedVersionGenerator,
	turnAuthHandler *TURNAuthHandler,
	bus psrpc.MessageBus,
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
		roomStore:         roomStore,
		telemetry:         telemetry,
		clientConfManager: clientConfManager,
		egressLauncher:    egressLauncher,
		agentClient:       agentClient,
		versionGenerator:  versionGenerator,
		turnAuthHandler:   turnAuthHandler,
		bus:               bus,

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
			if err := r.deleteRoom(ctx, livekit.RoomName(room.Name)); err != nil {
				return err
			}
		}
	}
	return nil
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
		for _, p := range room.GetParticipants() {
			_ = p.Close(true, types.ParticipantCloseReasonRoomManagerStop, false)
		}
		room.Close()
	}

	r.roomServers.Kill()
	r.participantServers.Kill()

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
					"room", roomName,
					"nodeID", r.currentNode.Id,
					"participant", pi.Identity,
					"reason", pi.ReconnectReason,
				)
				_ = responseSink.WriteMessage(&livekit.SignalResponse{
					Message: &livekit.SignalResponse_Leave{
						Leave: &livekit.LeaveRequest{
							CanReconnect: true,
							Reason:       livekit.DisconnectReason_STATE_MISMATCH,
						},
					},
				})
				return errors.New("could not restart closed participant")
			}

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
			if err = room.ResumeParticipant(
				participant,
				requestSource,
				responseSink,
				r.iceServersForParticipant(
					apiKey,
					participant,
					iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TLS,
				),
				pi.ReconnectReason,
			); err != nil {
				logger.Warnw("could not resume participant", err, "participant", pi.Identity)
				return err
			}
			r.telemetry.ParticipantResumed(ctx, room.ToProto(), participant.ToProto(), livekit.NodeID(r.currentNode.Id), pi.ReconnectReason)
			go r.rtcSessionWorker(room, participant, requestSource)
			return nil
		}

		// we need to clean up the existing participant, so a new one can join
		participant.GetLogger().Infow("removing duplicate participant")
		room.RemoveParticipant(participant.Identity(), participant.ID(), types.ParticipantCloseReasonDuplicateIdentity)
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
	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.GetLogger(), room.Name(), room.ID()),
		pi.Identity,
		sid,
		false)
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
		ProtocolVersion:         pv,
		Telemetry:               r.telemetry,
		Trailer:                 room.Trailer(),
		PLIThrottleConfig:       r.config.RTC.PLIThrottle,
		CongestionControlConfig: r.config.RTC.CongestionControl,
		PublishEnabledCodecs:    protoRoom.EnabledCodecs,
		SubscribeEnabledCodecs:  protoRoom.EnabledCodecs,
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
		ReconnectOnDataChannelError:  reconnectOnDataChannelError,
		DataChannelMaxBufferedAmount: r.config.RTC.DataChannelMaxBufferedAmount,
		VersionGenerator:             r.versionGenerator,
		TrackResolver:                room.ResolveMediaTrackForSubscriber,
		SubscriberAllowPause:         subscriberAllowPause,
		SubscriptionLimitAudio:       r.config.Limit.SubscriptionLimitAudio,
		SubscriptionLimitVideo:       r.config.Limit.SubscriptionLimitVideo,
		PlayoutDelay:                 roomInternal.GetPlayoutDelay(),
		SyncStreams:                  roomInternal.GetSyncStreams(),
	})
	if err != nil {
		return err
	}
	iceConfig := r.setIceConfig(participant)

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

	participantTopic := rpc.FormatParticipantTopic(roomName, participant.Identity())
	participantServer := utils.Must(rpc.NewTypedParticipantServer(r, r.bus))
	killParticipantServer := r.participantServers.Replace(participantTopic, participantServer)
	if err := participantServer.RegisterAllParticipantTopics(participantTopic); err != nil {
		killParticipantServer()
		pLogger.Errorw("could not join register participant topic", err)
		_ = participant.Close(true, types.ParticipantCloseReasonMessageBusFailed, false)
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
	participant.OnClose(func(p types.LocalParticipant) {
		killParticipantServer()

		if err := r.roomStore.DeleteParticipant(ctx, roomName, p.Identity()); err != nil {
			pLogger.Errorw("could not delete participant", err)
		}

		// update room store with new numParticipants
		proto := room.ToProto()
		persistRoomForParticipantCount(proto)
		r.telemetry.ParticipantLeft(ctx, proto, p.ToProto(), true)
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
	newRoom := rtc.NewRoom(ri, internal, *r.rtcConfig, &r.config.Audio, r.serverInfo, r.telemetry, r.agentClient, r.egressLauncher)

	roomTopic := rpc.FormatRoomTopic(roomName)
	roomServer := utils.Must(rpc.NewTypedRoomServer(r, r.bus))
	killRoomServer := r.roomServers.Replace(roomTopic, roomServer)
	if err := roomServer.RegisterAllRoomTopics(roomTopic); err != nil {
		killRoomServer()
		r.lock.Unlock()
		return nil, err
	}

	newRoom.OnClose(func() {
		killRoomServer()

		roomInfo := newRoom.ToProto()
		r.telemetry.RoomEnded(ctx, roomInfo)
		prometheus.RoomEnded(time.Unix(roomInfo.CreationTime, 0))
		if err := r.deleteRoom(ctx, roomName); err != nil {
			newRoom.Logger.Errorw("could not delete room", err)
		}

		newRoom.Logger.Infow("room closed")
	})

	newRoom.OnRoomUpdated(func() {
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
	stateCheckTicker := time.NewTicker(time.Millisecond * 500)
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
				pLogger.Errorw("could not refresh token", err, "connID", requestSource.ConnectionID())
			}
		case obj := <-requestSource.ReadChan():
			// In single node mode, the request source is directly tied to the signal message channel
			// this means ICE restart isn't possible in single node mode
			if obj == nil {
				if room.GetParticipantRequestSource(participant.Identity()) == requestSource {
					participant.HandleSignalSourceClose()
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
	switch rm := msg.Message.(type) {
	case *livekit.RTCNodeMessage_RemoveParticipant:
		r.RemoveParticipant(ctx, rm.RemoveParticipant)
	case *livekit.RTCNodeMessage_MuteTrack:
		r.MutePublishedTrack(ctx, rm.MuteTrack)
	case *livekit.RTCNodeMessage_UpdateParticipant:
		r.UpdateParticipant(ctx, rm.UpdateParticipant)
	case *livekit.RTCNodeMessage_DeleteRoom:
		r.DeleteRoom(ctx, rm.DeleteRoom)
	case *livekit.RTCNodeMessage_UpdateSubscriptions:
		r.UpdateSubscriptions(ctx, rm.UpdateSubscriptions)
	case *livekit.RTCNodeMessage_SendData:
		r.SendData(ctx, rm.SendData)
	case *livekit.RTCNodeMessage_UpdateRoomMetadata:
		r.UpdateRoomMetadata(ctx, rm.UpdateRoomMetadata)
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
	track := participant.SetTrackMuted(livekit.TrackID(req.TrackSid), req.Muted, true)
	return &livekit.MuteRoomTrackResponse{Track: track}, nil
}

func (r *RoomManager) UpdateParticipant(ctx context.Context, req *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error) {
	room, participant, err := r.roomAndParticipantForReq(ctx, req)
	if err != nil {
		return nil, err
	}

	participant.GetLogger().Debugw("updating participant",
		"metadata", req.Metadata, "permission", req.Permission)
	room.UpdateParticipantMetadata(participant, req.Name, req.Metadata)
	if req.Permission != nil {
		participant.SetPermission(req.Permission)
	}
	return participant.ToProto(), nil
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
		room.Logger.Infow("deleting room")
		for _, p := range room.GetParticipants() {
			_ = p.Close(true, types.ParticipantCloseReasonServiceRequestDeleteRoom, false)
		}
		room.Close()
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

	room.Logger.Debugw("api send data", "size", len(req.Data))
	up := &livekit.UserPacket{
		Payload:               req.Data,
		DestinationSids:       req.DestinationSids,
		DestinationIdentities: req.DestinationIdentities,
		Topic:                 req.Topic,
	}
	room.SendDataPacket(up, req.Kind)
	return &livekit.SendDataResponse{}, nil
}

func (r *RoomManager) UpdateRoomMetadata(ctx context.Context, req *livekit.UpdateRoomMetadataRequest) (*livekit.Room, error) {
	room := r.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	room.Logger.Debugw("updating room")
	room.SetMetadata(req.Metadata)
	return room.ToProto(), nil
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
			is := &livekit.ICEServer{}
			if s.AuthURI != "" {
				err := temporaryAuthForTurn(s, is)
				if err != nil {
					logger.Errorw("Temporary auth request failed, falling back to config-read ", err)
					continue
				}
			} else {
				configAuthRead(s, is)
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

func temporaryAuthForTurn(s config.TURNServer, is *livekit.ICEServer) error {
	req, err := http.NewRequest("GET", s.AuthURI, nil)
	if err != nil {
		logger.Errorw("could not create request", err)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Errorw("error making http request", err)
	}

	defer func(body io.ReadCloser) {
		err := body.Close()
		if err != nil {
			logger.Errorw("could not close response reader", err)
		}
	}(res.Body)

	body, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		logger.Errorw("could not read response", readErr)
	}

	var response struct {
		Username   string   `json:"username"`
		Credential string   `json:"credential"`
		TTL        int      `json:"ttl"`
		URIs       []string `json:"uris"`
	}

	err = json.Unmarshal(body, &response)

	is = &livekit.ICEServer{
		Urls:       response.URIs,
		Username:   response.Username,
		Credential: response.Credential,
	}

	if err != nil {
		return err
	}
	return nil
}

func configAuthRead(s config.TURNServer, is *livekit.ICEServer) {
	scheme := "turn"
	transport := "tcp"
	if s.Protocol == "tls" {
		scheme = "turns"
	} else if s.Protocol == "udp" {
		transport = "udp"
	}
	is = &livekit.ICEServer{
		Urls: []string{
			fmt.Sprintf("%s:%s:%d?transport=%s", scheme, s.Host, s.Port, transport),
		},
		Username:   s.Username,
		Credential: s.Credential,
	}

}
