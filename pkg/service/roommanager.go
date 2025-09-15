package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/rtc/relay"
	"github.com/livekit/livekit-server/pkg/rtc/relay/pc"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/version"

	"github.com/livekit/livekit-server/pkg/clientconfiguration"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry"

	"github.com/livekit/livekit-server/pkg/config"
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
	relayRtcConfig    *relay.WebRTCRelayConfig
	serverInfo        *livekit.ServerInfo
	currentNode       routing.LocalNode
	router            routing.Router
	roomStore         ObjectStore
	telemetry         telemetry.TelemetryService
	clientConfManager clientconfiguration.ClientConfigurationManager
	egressLauncher    rtc.EgressLauncher
	versionGenerator  utils.TimedVersionGenerator

	rooms               map[livekit.RoomKey]*rtc.Room
	outRelayCollections map[livekit.RoomKey]*relay.Collection
	inRelayCollections  map[livekit.RoomKey]*relay.Collection

	iceConfigCache map[livekit.ParticipantID]*iceConfigCacheEntry
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

	relayRtcConf, err := relay.NewWebRTCRelayConfig(conf, currentNode.Ip)
	if err != nil {
		return nil, err
	}

	r := &RoomManager{
		config:            conf,
		rtcConfig:         rtcConf,
		relayRtcConfig:    relayRtcConf,
		currentNode:       currentNode,
		router:            router,
		roomStore:         roomStore,
		telemetry:         telemetry,
		clientConfManager: clientConfManager,
		egressLauncher:    egressLauncher,
		versionGenerator:  versionGenerator,

		rooms:               make(map[livekit.RoomKey]*rtc.Room),
		outRelayCollections: make(map[livekit.RoomKey]*relay.Collection),
		inRelayCollections:  make(map[livekit.RoomKey]*relay.Collection),

		iceConfigCache: make(map[livekit.ParticipantID]*iceConfigCacheEntry),

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

func (r *RoomManager) GetRoom(_ context.Context, roomKey livekit.RoomKey) *rtc.Room {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.rooms[roomKey]
}

// DeleteRoom completely deletes all room information, including active sessions, room store, and routing info
func (r *RoomManager) DeleteRoom(ctx context.Context, roomKey livekit.RoomKey) error {
	logger.Infow("Deleting room state", "room", roomKey, "nodeID", r.currentNode.Id)
	r.lock.Lock()
	delete(r.rooms, roomKey)
	delete(r.outRelayCollections, roomKey)
	delete(r.inRelayCollections, roomKey)
	r.lock.Unlock()

	var err, err2 error
	wg := sync.WaitGroup{}
	wg.Add(2)
	// clear routing information
	go func() {
		defer wg.Done()
		err = r.router.ClearRoomState(ctx, roomKey)
	}()
	// also delete room from db
	go func() {
		defer wg.Done()
		err2 = r.roomStore.DeleteRoom(ctx, roomKey)
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
			if err := r.DeleteRoom(ctx, livekit.RoomKey(room.Key)); err != nil {
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
	roomKey livekit.RoomKey,
	pi routing.ParticipantInit,
	requestSource routing.MessageSource,
	responseSink routing.MessageSink,
) error {

	room, outRelayCollection, _, err := r.getOrCreateRoom(ctx, roomKey)
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
			logger.Infow("Resuming RTC session",
				"room", roomKey,
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
				logger.Warnw("Could not resume participant", err, "participant", pi.Identity)
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

	logger.Debugw("Starting RTC session",
		"room", roomKey,
		"nodeID", r.currentNode.Id,
		"participant", pi.Identity,
		"sdk", pi.Client.Sdk,
		"sdkVersion", pi.Client.Version,
		"protocol", pi.Client.Protocol,
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
		ApiKey:                  pi.ApiKey,
		Limit:                   pi.Limit,
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
		BandwidthChecker: func(apiKey livekit.ApiKey, limit int64) bool {
			return true
		},
		RelayCollection: outRelayCollection,
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
	if err = r.roomStore.StoreParticipant(ctx, roomKey, participant.ToProto()); err != nil {
		pLogger.Errorw("could not store participant", err)
	}

	persistRoomForParticipantCount := func(proto *livekit.Room) {
		if !participant.Hidden() {
			err = r.roomStore.StoreRoom(ctx, proto, livekit.RoomKey(proto.Key), room.Internal())
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
		if err := r.roomStore.DeleteParticipant(ctx, roomKey, p.Identity()); err != nil {
			pLogger.Errorw("could not delete participant", err)
		}

		// update room store with new numParticipants
		proto := room.ToProto()
		persistRoomForParticipantCount(proto)
		r.telemetry.ParticipantLeft(ctx, proto, p.ToProto(), true, p.ClaimGrants().WebHookURL)

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
		r.iceConfigCache[participant.ID()] = &iceConfigCacheEntry{
			iceConfig:  iceConfig,
			modifiedAt: time.Now(),
		}
		r.lock.Unlock()
	})

	go r.rtcSessionWorker(room, participant, requestSource)
	return nil
}

// create the actual room object, to be used on RTC node
func (r *RoomManager) getOrCreateRoom(ctx context.Context, roomKey livekit.RoomKey) (*rtc.Room, *relay.Collection, *relay.Collection, error) {
	r.lock.RLock()
	lastSeenRoom := r.rooms[roomKey]
	lastSeenOutRelayCollection := r.outRelayCollections[roomKey]
	lastSeenInRelayCollection := r.inRelayCollections[roomKey]
	r.lock.RUnlock()

	if lastSeenRoom != nil && lastSeenRoom.Hold() {
		return lastSeenRoom, lastSeenOutRelayCollection, lastSeenInRelayCollection, nil
	}

	// create new room, get details first
	ri, internal, roomCommunicator, err := r.roomStore.LoadRoom(ctx, roomKey, true)
	if err != nil {
		return nil, nil, nil, err
	}

	r.lock.Lock()

	currentRoom := r.rooms[roomKey]
	currentOutRelayCollection := r.outRelayCollections[roomKey]
	currentInRelayCollection := r.inRelayCollections[roomKey]

	for currentRoom != lastSeenRoom {
		r.lock.Unlock()
		if currentRoom != nil && currentRoom.Hold() {
			return currentRoom, currentOutRelayCollection, currentInRelayCollection, nil
		}

		lastSeenRoom = currentRoom
		r.lock.Lock()
		currentRoom = r.rooms[roomKey]
		currentOutRelayCollection = r.outRelayCollections[roomKey]
		currentInRelayCollection = r.inRelayCollections[roomKey]
	}

	outRelayCollection := relay.NewCollection()
	inRelayCollection := relay.NewCollection()
	rtcConfig := *r.rtcConfig
	relayRtcConfig := *r.relayRtcConfig

	// construct ice servers
	newRoom := rtc.NewRoom(ri, internal, rtcConfig, relayRtcConfig, &r.config.Audio, r.serverInfo, r.telemetry, r.egressLauncher)

	newRoom.OnClose(func() {
		roomInfo := newRoom.ToProto()

		// graceful close local relays
		r.CloseLocalRelayConnections(ctx, roomKey)

		if err := r.SendDeleteRoomMessage(ctx, roomKey); err != nil {
			newRoom.Logger.Errorw("Could not send graceful shutdown request to outgoing relays", err)
		}

		r.telemetry.RoomEnded(ctx, roomInfo)
		prometheus.RoomEnded(time.Unix(roomInfo.CreationTime, 0))
		if err := r.DeleteRoom(ctx, roomKey); err != nil {
			newRoom.Logger.Errorw("Could not delete room", err)
		}

		newRoom.Logger.Infow("Room closed", "roomID", roomInfo.Sid, "roomName", roomInfo.Name)
	})

	newRoom.OnMetadataUpdate(func(metadata string) {
		if err := r.roomStore.StoreRoom(ctx, newRoom.ToProto(), roomKey, newRoom.Internal()); err != nil {
			newRoom.Logger.Errorw("Could not handle metadata update", err)
		}
	})

	newRoom.OnParticipantChanged(func(p types.LocalParticipant) {
		if !p.IsDisconnected() {
			if err := r.roomStore.StoreParticipant(ctx, roomKey, p.ToProto()); err != nil {
				newRoom.Logger.Errorw("Could not handle participant change", err)
			}
		}
	})

	newRoom.OnSendParticipantUpdates(func(updates []*livekit.ParticipantInfo) {
		if len(updates) == 0 {
			return
		}

		if updatesForRelay, err := getUpdatesPayloadForRelay(newRoom, updates); err != nil {
			newRoom.Logger.Errorw("Could not create updates for relay", err)
		} else if len(updatesForRelay) > 0 {
			outRelayCollection.ForEach(func(relay relay.Relay) {
				if err := relay.SendMessage(updatesForRelay); err != nil {
					newRoom.Logger.Errorw("Could not send participant updates to relay", err)
				}
			})
		}
	})

	newRoom.OnBroadcastDataPacket(func(dp *livekit.DataPacket) {
		dpData, err := proto.Marshal(dp)
		if err != nil {
			newRoom.Logger.Errorw("Could not marshal data packet", err)
			return
		}
		if messageData, err := json.Marshal(relayMessage{DataPacket: dpData}); err != nil {
			newRoom.Logger.Errorw("Could not create data packet message for relay", err)
		} else {
			outRelayCollection.ForEach(func(relay relay.Relay) {
				if err := relay.SendMessage(messageData); err != nil {
					newRoom.Logger.Errorw("Could not send data packet to relay", err, "relayId", relay.ID())
				}
			})
		}
	})

	newRoom.OnSpeakersChanged(func(speakers []*livekit.SpeakerInfo) {
		if len(speakers) == 0 {
			return
		}

		if speakersForRelay, err := getSpeakersPayloadForRelay(newRoom, speakers); err != nil {
			newRoom.Logger.Errorw("Could not create speakers payload for relay", err)
		} else if len(speakersForRelay) > 0 {
			outRelayCollection.ForEach(func(relay relay.Relay) {
				if err := relay.SendMessage(speakersForRelay); err != nil {
					newRoom.Logger.Errorw("Could not send speakers to relay", err)
				}
			})
		}
	})

	newRoom.OnConnectionQualityInfos(func(qualityInfos map[livekit.ParticipantID]*livekit.ConnectionQualityInfo) {
		if len(qualityInfos) == 0 {
			return
		}

		if connQualitiesForRelay, err := getConnQualitiesPayloadForRelay(newRoom, qualityInfos); err != nil {
			newRoom.Logger.Errorw("Could not create connection qualities payload for relay", err)
		} else if len(connQualitiesForRelay) > 0 {
			outRelayCollection.ForEach(func(relay relay.Relay) {
				if err := relay.SendMessage(connQualitiesForRelay); err != nil {
					newRoom.Logger.Errorw("Could not send connection qualities to relay", err)
				}
			})
		}
	})

	pendingAnswers := map[string]chan []byte{}
	pendingAnswersMu := sync.Mutex{}

	roomCommunicator.ForEachPeer(func(peerId string) {
		logger.Infow("New p2p peer", "peerId", peerId, "roomKey", roomKey, "nodeID", r.currentNode.Id)
		rel, err := pc.NewRelay(newRoom.Logger, &relay.RelayConfig{
			ID:            peerId,
			BufferFactory: newRoom.GetBufferFactory(),
			SettingEngine: relayRtcConfig.SettingEngine,
			ICEServers:    relayRtcConfig.Configuration.ICEServers,
			RelayUDPMux:   relayRtcConfig.RelayUDPMux,
			RelayUdpPort:  relayRtcConfig.RelayUdpPort,
			Side:          "out",
		})
		if err != nil {
			logger.Errorw("Failed to create out relay", err, "peerId", peerId, "roomKey", roomKey, "nodeID", r.currentNode.Id)
			return
		}

		rel.OnReady(func() {
			logger.Infow("Out relay is ready", "relayID", rel.ID(), "roomKey", roomKey, "nodeID", r.currentNode.Id)

			// todo: combine messages
			updates := rtc.ToProtoParticipants(newRoom.GetParticipants())
			if len(updates) > 0 {
				if updatesForRelay, err := getUpdatesPayloadForRelay(newRoom, updates); err != nil {
					newRoom.Logger.Errorw("failed to create participant update for relay", err, "relayID", rel.ID(), "roomID", newRoom.ID())
				} else if err := rel.SendMessage(updatesForRelay); err != nil {
					newRoom.Logger.Errorw("failed to send participant updates to relay", err, "relayID", rel.ID(), "roomID", newRoom.ID())
				}
			}

			// todo: combine messages
			speakers := newRoom.GetActiveSpeakers()
			if len(speakers) > 0 {
				if speakersForRelay, err := getSpeakersPayloadForRelay(newRoom, speakers); err != nil {
					newRoom.Logger.Errorw("failed to create speakers payload for relay", err, "relayID", rel.ID(), "roomID", newRoom.ID())
				} else if err := rel.SendMessage(speakersForRelay); err != nil {
					newRoom.Logger.Errorw("failed to send speakers to relay", err, "relayID", rel.ID(), "roomID", newRoom.ID())
				}
			}

			// todo: combine messages
			connQualities := newRoom.GetConnectionInfos()
			if len(connQualities) > 0 {
				if connQualitiesForRelay, err := getConnQualitiesPayloadForRelay(newRoom, connQualities); err != nil {
					newRoom.Logger.Errorw("failed to create connection qualities payload for relay", err, "relayID", rel.ID(), "roomID", newRoom.ID())
				} else if err := rel.SendMessage(connQualitiesForRelay); err != nil {
					newRoom.Logger.Errorw("failed to send connection qualities to relay", err, "relayID", rel.ID(), "roomID", newRoom.ID())
				}
			}

			outRelayCollection.AddRelay(rel)
		})

		rel.OnConnectionStateChange(func(state webrtc.ICEConnectionState) {
			logger.Infow("Out relay connection state changed", "state", state.String(), "relayID", rel.ID(), "roomKey", roomKey, "nodeID", r.currentNode.Id)
		})

		signalFn := func(offer []byte) ([]byte, error) {
			answer := make(chan []byte, 1)

			pendingAnswersMu.Lock()
			msgId, sendErr := roomCommunicator.SendMessage(peerId, packSignalPeerMessage("", offer))
			if sendErr != nil {
				pendingAnswersMu.Unlock()
				return nil, fmt.Errorf("cannot send message to room commmunicator: %w", sendErr)
			}
			logger.Infow("Offer sent", "peerId", peerId, "relayID", rel.ID(), "roomKey", roomKey)
			pendingAnswers[msgId] = answer
			pendingAnswersMu.Unlock()

			defer func() {
				pendingAnswersMu.Lock()
				delete(pendingAnswers, msgId)
				pendingAnswersMu.Unlock()
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			select {
			case a := <-answer:
				return a, nil
			case <-ctx.Done():
				return nil, fmt.Errorf("cannot receive answer: %w", ctx.Err())
			}
		}
		if err := rel.Offer(signalFn); err != nil {
			logger.Errorw("Failed to create relay offer", err, "peerId", peerId, "relayID", rel.ID(), "roomKey", roomKey)
		}
	})

	roomCommunicator.OnMessage(func(message interface{}, fromPeerId string, eventId string) {
		replyTo, signal, err := unpackSignalPeerMessage(message)
		if err != nil {
			logger.Errorw("Failed to unmarshal signal peer message", err, "fromPeerId", fromPeerId, "roomKey", roomKey)
			return
		}
		if len(replyTo) > 0 {
			// Answer
			pendingAnswersMu.Lock()
			if answer, ok := pendingAnswers[replyTo]; ok {
				answer <- signal
			}
			pendingAnswersMu.Unlock()
		} else {
			// Offer
			rel, err := pc.NewRelay(newRoom.Logger, &relay.RelayConfig{
				ID:            fromPeerId,
				BufferFactory: newRoom.GetBufferFactory(),
				SettingEngine: relayRtcConfig.SettingEngine,
				ICEServers:    relayRtcConfig.Configuration.ICEServers,
				RelayUDPMux:   relayRtcConfig.RelayUDPMux,
				RelayUdpPort:  relayRtcConfig.RelayUdpPort,
				Side:          "in",
			})
			if err != nil {
				logger.Errorw("Failed to create in-relay", err, "fromPeerId", fromPeerId, "roomKey", roomKey, "nodeID", r.currentNode.Id)
				return
			}

			rel.OnReady(func() {
				logger.Infow("In-relay is ready", "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomKey", roomKey, "nodeID", r.currentNode.Id)
				inRelayCollection.AddRelay(rel)
				// TODO
			})

			rel.OnConnectionStateChange(func(state webrtc.ICEConnectionState) {
				logger.Infow("In-relay connection state changed", "state", state.String(), "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomKey", roomKey, "nodeID", r.currentNode.Id)
			})

			answer, answerErr := rel.Answer(signal)
			if answerErr != nil {
				logger.Errorw("Failed to create in-relay answer", answerErr, "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomKey", roomKey)
				return
			}

			if _, err := roomCommunicator.SendMessage(fromPeerId, packSignalPeerMessage(eventId, answer)); err != nil {
				logger.Errorw("Failed to send answer", err, "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomKey", roomKey)
				return
			}

			logger.Infow("Answer sent", "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomKey", roomKey)

			rel.OnMessage(func(id uint64, payload []byte) {
				logger.Debugw("Relay message received", "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomKey", roomKey)
				var msg relayMessage

				if err := json.Unmarshal(payload, &msg); err != nil {
					newRoom.Logger.Errorw("Failed to unmarshal relay message", err, "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomID", newRoom.ID())
					return
				}
				if len(msg.Updates) > 0 {
					for _, update := range msg.Updates {
						r.onRelayParticipantUpdate(newRoom, rel, update)
					}
					for _, op := range newRoom.GetParticipants() {
						if _, ok := op.(*rtc.RelayedParticipantImpl); ok {
							continue
						}
						if err := op.SendParticipantUpdate(msg.Updates); err != nil {
							newRoom.Logger.Errorw("Failed to send update to participant", err,
								"participant", op.Identity(), "pID", op.ID(), "roomID", newRoom.ID())
						}
					}
				}
				if len(msg.DataPacket) > 0 {
					dp := livekit.DataPacket{}
					if err := proto.Unmarshal(msg.DataPacket, &dp); err != nil {
						newRoom.Logger.Errorw("Failed to unmarshal relay data packet", err, "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomID", newRoom.ID())
						return
					} else {
						rtc.BroadcastDataPacketForRoom(newRoom, nil, &dp, newRoom.Logger)
					}
				}
				if len(msg.Speakers) > 0 {
					r.onRelaySpeakersChanged(newRoom, msg.Speakers)
				}
				if len(msg.ConnQualities) > 0 {
					r.onRelayConnectionQualities(newRoom, msg.ConnQualities)
				}
			})

			rel.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, mid string, rid string, meta []byte) {
				logger.Infow("Relay track published", "mid", mid, "rid", rid, "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomKey", roomKey)
				var addTrackSignal relay.AddTrackSignal
				if err := json.Unmarshal(meta, &addTrackSignal); err != nil {
					newRoom.Logger.Errorw("Failed to unmarshal add track signal", err, "relayID", rel.ID(), "fromPeerId", fromPeerId, "roomID", newRoom.ID())
					return
				}
				onRelayAddTrack(newRoom, track, receiver, mid, rid, addTrackSignal)
			})
		}
	})

	r.rooms[roomKey] = newRoom
	r.outRelayCollections[roomKey] = outRelayCollection
	r.inRelayCollections[roomKey] = inRelayCollection

	r.lock.Unlock()

	newRoom.Hold()

	r.telemetry.RoomStarted(ctx, newRoom.ToProto())
	prometheus.RoomStarted()

	return newRoom, outRelayCollection, inRelayCollection, nil
}

// manages an RTC session for a participant, runs on the RTC node
func (r *RoomManager) rtcSessionWorker(room *rtc.Room, participant types.LocalParticipant, requestSource routing.MessageSource) {
	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.GetLogger(), room.Key(), room.ID()),
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
func (r *RoomManager) handleRTCMessage(ctx context.Context, roomKey livekit.RoomKey, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) {
	r.lock.RLock()
	room := r.rooms[roomKey]
	r.lock.RUnlock()

	if room == nil {
		if _, ok := msg.Message.(*livekit.RTCNodeMessage_DeleteRoom); ok {
			// special case of a non-RTC room e.g. room created but no participants joined
			logger.Debugw("Deleting non-rtc room", "room", roomKey, "nodeID", r.currentNode.Id)
			err := r.roomStore.DeleteRoom(ctx, roomKey)
			if err != nil {
				logger.Debugw("Failed to delete non-rtc room", "err", err, "room", roomKey, "nodeID", r.currentNode.Id)
			}
			return
		} else {
			logger.Warnw("Could not find room", nil, "room", roomKey, "nodeID", r.currentNode.Id)
			return
		}
	}

	participant := room.GetParticipant(identity)
	var sid livekit.ParticipantID
	if participant != nil {
		sid = participant.ID()
	}
	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.GetLogger(), roomKey, room.ID()),
		identity,
		sid,
		false,
	)

	switch rm := msg.Message.(type) {
	case *livekit.RTCNodeMessage_RemoveParticipant:
		if participant == nil {
			return
		}
		pLogger.Infow("Removing participant", "identity", identity, "roomKey", roomKey, "nodeID", r.currentNode.Id)
		// remove participant by identity, any SID
		room.RemoveParticipant(identity, "", types.ParticipantCloseReasonServiceRequestRemoveParticipant)
	case *livekit.RTCNodeMessage_MuteTrack:
		if participant == nil {
			return
		}
		pLogger.Debugw("Setting track muted",
			"trackID", rm.MuteTrack.TrackSid, "muted", rm.MuteTrack.Muted, "participant", identity, "roomKey", roomKey)
		if !rm.MuteTrack.Muted && !r.config.Room.EnableRemoteUnmute {
			pLogger.Errorw("Cannot unmute track, remote unmute is disabled", nil, "trackID", rm.MuteTrack.TrackSid, "participant", identity, "roomKey", roomKey)
			return
		}
		participant.SetTrackMuted(livekit.TrackID(rm.MuteTrack.TrackSid), rm.MuteTrack.Muted, true)
	case *livekit.RTCNodeMessage_UpdateParticipant:
		if participant == nil {
			return
		}
		pLogger.Debugw("Updating participant", "metadata", rm.UpdateParticipant.Metadata,
			"permission", rm.UpdateParticipant.Permission, "identity", identity, "roomKey", roomKey)
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
		room.Logger.Infow("Deleting room", "roomKey", roomKey, "roomID", room.ID(), "nodeID", r.currentNode.Id)

		r.CloseLocalRelayConnections(ctx, roomKey)

		for _, p := range room.GetParticipants() {
			_ = p.Close(true, types.ParticipantCloseReasonServiceRequestDeleteRoom)
		}
		room.Close()
	case *livekit.RTCNodeMessage_UpdateSubscriptions:
		if participant == nil {
			return
		}
		pLogger.Debugw("Updating participant subscriptions", "participant", identity, "roomKey", roomKey)
		room.UpdateSubscriptions(
			participant,
			livekit.StringsAsTrackIDs(rm.UpdateSubscriptions.TrackSids),
			rm.UpdateSubscriptions.ParticipantTracks,
			rm.UpdateSubscriptions.Subscribe,
		)
	case *livekit.RTCNodeMessage_SendData:
		pLogger.Debugw("API send data", "size", len(rm.SendData.Data), "kind", rm.SendData.Kind, "roomKey", roomKey)
		up := &livekit.UserPacket{
			Payload:         rm.SendData.Data,
			DestinationSids: rm.SendData.DestinationSids,
			Topic:           rm.SendData.Topic,
		}
		room.SendDataPacket(up, rm.SendData.Kind)
	case *livekit.RTCNodeMessage_UpdateRoomMetadata:
		pLogger.Debugw("Updating room metadata", "roomKey", roomKey, "roomID", room.ID())
		room.SetMetadata(rm.UpdateRoomMetadata.Metadata)
	}
}

func (r *RoomManager) iceServersForRoom(ri *livekit.Room, tlsOnly bool) []*livekit.ICEServer {
	var iceServers []*livekit.ICEServer
	rtcConf := r.config.RTC

	if tlsOnly && r.config.TURN.TLSPort == 0 {
		logger.Warnw("TLS only enabled but no TURN TLS config", nil, "room", ri.Name, "roomID", ri.Sid)
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
				Username:   ri.Key,
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
	iceConfigCacheEntry, ok := r.iceConfigCache[participant.ID()]
	if !ok || time.Since(iceConfigCacheEntry.modifiedAt) > iceConfigTTL {
		delete(r.iceConfigCache, participant.ID())
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

func getUpdatesPayloadForRelay(room *rtc.Room, updates []*livekit.ParticipantInfo) ([]byte, error) {
	updatesForRelay := make([]*livekit.ParticipantInfo, 0, len(updates))
	for _, update := range updates {
		if _, ok := room.GetParticipant(livekit.ParticipantIdentity(update.Identity)).(*rtc.RelayedParticipantImpl); ok {
			continue
		}
		updatesForRelay = append(updatesForRelay, update)
	}

	if updatesPayload, err := json.Marshal(relayMessage{Updates: updatesForRelay}); err != nil {
		return nil, fmt.Errorf("could not marshal participant updates: %w", err)
	} else {
		return updatesPayload, nil
	}
}

func getSpeakersPayloadForRelay(room *rtc.Room, speakers []*livekit.SpeakerInfo) ([]byte, error) {
	speakersForRelay := make([]*livekit.SpeakerInfo, 0, len(speakers))
	for _, speaker := range speakers {
		if _, ok := room.GetParticipantByID(livekit.ParticipantID(speaker.GetSid())).(*rtc.RelayedParticipantImpl); ok {
			continue
		}
		speakersForRelay = append(speakersForRelay, speaker)
	}

	if speakersPayload, err := json.Marshal(relayMessage{Speakers: speakersForRelay}); err != nil {
		return nil, fmt.Errorf("could not marshal participant speakers: %w", err)
	} else {
		return speakersPayload, nil
	}
}

func getConnQualitiesPayloadForRelay(room *rtc.Room, connQualities map[livekit.ParticipantID]*livekit.ConnectionQualityInfo) ([]byte, error) {
	connQualitiesForRelay := make([]*livekit.ConnectionQualityInfo, 0, len(connQualities))
	for participantID, connQuality := range connQualities {
		if _, ok := room.GetParticipantByID(participantID).(*rtc.RelayedParticipantImpl); ok {
			continue
		}
		connQualitiesForRelay = append(connQualitiesForRelay, connQuality)
	}

	if connQualitiesPayload, err := json.Marshal(relayMessage{ConnQualities: connQualitiesForRelay}); err != nil {
		return nil, fmt.Errorf("could not marshal connection qualities: %w", err)
	} else {
		return connQualitiesPayload, nil
	}
}

func packSignalPeerMessage(replyTo string, signal []byte) interface{} {
	return &signalPeerMessage{
		ReplyTo: replyTo,
		Signal:  base64.StdEncoding.EncodeToString(signal),
	}
}

func unpackSignalPeerMessage(message interface{}) (replyTo string, signal []byte, err error) {
	messageMap, ok := message.(map[string]interface{})
	if !ok {
		err = errors.New("cannot cast")
		return
	}

	replyToValue, ok := messageMap["replyTo"]
	if !ok {
		err = errors.New("ReplyTo undefined")
		return
	}
	replyTo, ok = replyToValue.(string)
	if !ok {
		err = errors.New("cannot cast ReplyTo to string")
		return
	}

	signalBase64Value, ok := messageMap["signal"]
	if !ok {
		err = errors.New("Signal undefined")
		return
	}
	signalBase64, ok := signalBase64Value.(string)
	if !ok {
		err = errors.New("cannot cast Signal to string")
		return
	}
	signal, err = base64.StdEncoding.DecodeString(signalBase64)

	return
}

func (r *RoomManager) onRelayParticipantUpdate(room *rtc.Room, rel relay.Relay, pi *livekit.ParticipantInfo) {
	participantIdentity := livekit.ParticipantIdentity(pi.Identity)

	if pi.State == livekit.ParticipantInfo_DISCONNECTED {
		room.RemoveParticipant(participantIdentity, livekit.ParticipantID(pi.Sid), types.ParticipantCloseReasonStateDisconnected)
	} else {
		participant := room.GetParticipant(participantIdentity)
		if participant == nil {
			rtcConfig := room.Config
			rtcConfig.SetBufferFactory(rel.GetBufferFactory())

			participant, _ = rtc.NewRelayedParticipant(rtc.RelayedParticipantParams{
				Identity:    participantIdentity,
				Name:        livekit.ParticipantName(pi.Name),
				SID:         livekit.ParticipantID(pi.Sid),
				Config:      &rtcConfig,
				AudioConfig: r.config.Audio,
				VideoConfig: r.config.Video,
				Logger:      rtc.LoggerWithParticipant(room.Logger, livekit.ParticipantIdentity(pi.Identity), livekit.ParticipantID(pi.Sid), true),
				// SimTracks: nil,
				// InitialVersion: 0,
				Telemetry: r.telemetry,
				PLIThrottleConfig: config.PLIThrottleConfig{
					LowQuality:  500 * time.Millisecond,
					MidQuality:  time.Second,
					HighQuality: time.Second,
				},
				VersionGenerator: utils.NewDefaultTimedVersionGenerator(),
				Relay:            rel,
			})
			opts := rtc.ParticipantOptions{
				AutoSubscribe: false,
			}
			if err := room.Join(participant, nil, &opts, nil); err != nil {
				logger.Errorw("Failed to join remote participant", err, "identity", participant.Identity(), "roomKey", room.Key(), "roomID", room.ID())
				return
			}
			if err := r.roomStore.StoreParticipant(context.TODO(), room.Key(), participant.ToProto()); err != nil {
				logger.Errorw("Failed to store remote participant", err, "identity", participant.Identity(), "roomKey", room.Key(), "roomID", room.ID())
			}

			clientMeta := &livekit.AnalyticsClientMeta{Region: r.currentNode.Region, Node: r.currentNode.Id}
			r.telemetry.ParticipantJoined(context.TODO(), room.ToProto(), participant.ToProto(), nil, clientMeta, true)
			participant.OnClose(func(p types.LocalParticipant, m map[livekit.TrackID]livekit.ParticipantID) {
				if err := r.roomStore.DeleteParticipant(context.TODO(), room.Key(), p.Identity()); err != nil {
					logger.Errorw("Failed to delete remote participant", err, "identity", p.Identity(), "roomKey", room.Key(), "roomID", room.ID())
				}
				r.telemetry.ParticipantLeft(context.TODO(), room.ToProto(), p.ToProto(), true, p.ClaimGrants().WebHookURL)
			})
			logger.Infow("Remote participant joined", "identity", participant.Identity(), "roomKey", room.Key(), "roomID", room.ID())
		} else if _, ok := participant.(*rtc.RelayedParticipantImpl); !ok {
			logger.Errorw("Non-relayed participant is already joined", nil, "identity", participant.Identity(), "roomKey", room.Key(), "roomID", room.ID())
			return
		}
		relayedParticipant := participant.(*rtc.RelayedParticipantImpl)
		relayedParticipant.SetName(pi.Name)
		relayedParticipant.UpdateState(pi.State)
		relayedParticipant.SetMetadata(pi.Metadata)
	}
}

func (r *RoomManager) onRelaySpeakersChanged(room *rtc.Room, speakers []*livekit.SpeakerInfo) {
	for _, speaker := range speakers {
		if relayedParticipant, ok := room.GetParticipantByID(livekit.ParticipantID(speaker.GetSid())).(*rtc.RelayedParticipantImpl); !ok {
			continue
		} else {
			relayedParticipant.SetAudioLevel(float64(speaker.GetLevel()), speaker.GetActive())
		}
	}
}

func (r *RoomManager) onRelayConnectionQualities(room *rtc.Room, connQualities []*livekit.ConnectionQualityInfo) {
	for _, connQuality := range connQualities {
		if relayedParticipant, ok := room.GetParticipantByID(livekit.ParticipantID(connQuality.GetParticipantSid())).(*rtc.RelayedParticipantImpl); !ok {
			continue
		} else {
			relayedParticipant.SetConnectionQuality(connQuality)
		}
	}
}

func onRelayAddTrack(room *rtc.Room, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, mid string, rid string, addTrackSignal relay.AddTrackSignal) {
	participantIdentity := livekit.ParticipantIdentity(addTrackSignal.Identity)

	if relayedParticipant, ok := room.GetParticipant(participantIdentity).(*rtc.RelayedParticipantImpl); ok {
		relayedParticipant.OnMediaTrack(track, receiver, mid, rid, addTrackSignal.Track)
	} else {
		room.Logger.Errorw("Unknown relayed participant", nil, "identity", participantIdentity, "roomID", room.ID())
	}
}

type relayMessage struct {
	Updates       []*livekit.ParticipantInfo       `json:"updates,omitempty"`
	DataPacket    []byte                           `json:"dataPacket,omitempty"`
	Speakers      []*livekit.SpeakerInfo           `json:"speakers,omitempty"`
	ConnQualities []*livekit.ConnectionQualityInfo `json:"connQualities,omitempty"`
}

type signalPeerMessage struct {
	ReplyTo string `json:"replyTo"`
	Signal  string `json:"signal"`
}

func (r *RoomManager) SendDeleteRoomMessage(ctx context.Context, roomKey livekit.RoomKey) error {
	shutdownMsg := &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_DeleteRoom{
			DeleteRoom: &livekit.DeleteRoomRequest{
				Room: string(roomKey),
			},
		},
		SenderTime: time.Now().Unix(),
	}

	return r.router.WriteRoomRTC(ctx, roomKey, shutdownMsg)
}

func (r *RoomManager) CloseLocalRelayConnections(ctx context.Context, roomKey livekit.RoomKey) {
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	outRelayCollection := r.outRelayCollections[roomKey]
	if outRelayCollection != nil {
		outRelayCollection.ForEach(func(rel relay.Relay) {
			wg.Add(1)
			go func(rel relay.Relay) {
				defer wg.Done()
				if pr, ok := rel.(*pc.PcRelay); ok {
					pr.Close()
					select {
					case <-pr.Closed():
					case <-ctx.Done():
					}
				} else {
					rel.Close()
				}
			}(rel)
		})
	}

	inRelayCollection := r.inRelayCollections[roomKey]
	if inRelayCollection != nil {
		inRelayCollection.ForEach(func(rel relay.Relay) {
			wg.Add(1)
			go func(rel relay.Relay) {
				defer wg.Done()
				if pr, ok := rel.(*pc.PcRelay); ok {
					pr.Close()
					select {
					case <-pr.Closed():
					case <-ctx.Done():
					}
				} else {
					rel.Close()
				}
			}(rel)
		})
	}

	wg.Wait()
}
