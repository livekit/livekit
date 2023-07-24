package routing

import (
	"context"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"
	"log"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/livekit/livekit-server/pkg/p2p"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	// expire participant mappings after a day
	participantMappingTTL = 24 * time.Hour
	statsUpdateInterval   = 2 * time.Second
	statsMaxDelaySeconds  = 30
)

// aggregated channel for all participants
const localRTCChannelSize = 10000

// a router of messages on the same node, basic implementation for local testing
type LocalRouter struct {
	currentNode  LocalNode
	signalClient SignalClient

	lock sync.RWMutex
	// channels for each participant
	requestChannels  map[string]*MessageChannel
	responseChannels map[string]*MessageChannel
	isStarted        atomic.Bool

	rtcMessageChan *MessageChannel

	onNewParticipant NewParticipantCallback
	onRTCMessage     RTCMessageCallback

	mainDatabase        *p2p_database.DB
	routerCommunicators map[livekit.RoomKey]*p2p.RouterCommunicatorImpl
}

func NewLocalRouter(currentNode LocalNode, signalClient SignalClient, mainDatabase *p2p_database.DB) *LocalRouter {
	return &LocalRouter{
		mainDatabase:        mainDatabase,
		currentNode:         currentNode,
		signalClient:        signalClient,
		requestChannels:     make(map[string]*MessageChannel),
		responseChannels:    make(map[string]*MessageChannel),
		rtcMessageChan:      NewMessageChannel(localRTCChannelSize),
		routerCommunicators: make(map[livekit.RoomKey]*p2p.RouterCommunicatorImpl),
	}
}

func (r *LocalRouter) GetNodeForRoom(_ context.Context, _ livekit.RoomKey) (*livekit.Node, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	node := proto.Clone((*livekit.Node)(r.currentNode)).(*livekit.Node)
	return node, nil
}

func (r *LocalRouter) SetNodeForRoom(_ context.Context, _ livekit.RoomKey, _ livekit.NodeID) error {
	return nil
}

func (r *LocalRouter) ClearRoomState(_ context.Context, roomKey livekit.RoomKey) error {

	db, exists := r.routerCommunicators[roomKey]
	if exists {
		db.Close()
	}

	delete(r.routerCommunicators, roomKey)

	return nil
}

func (r *LocalRouter) RegisterNode() error {
	return nil
}

func (r *LocalRouter) UnregisterNode() error {
	return nil
}

func (r *LocalRouter) RemoveDeadNodes() error {
	return nil
}

func (r *LocalRouter) GetNode(nodeID livekit.NodeID) (*livekit.Node, error) {
	if nodeID == livekit.NodeID(r.currentNode.Id) {
		return r.currentNode, nil
	}
	return nil, ErrNotFound
}

func (r *LocalRouter) ListNodes() ([]*livekit.Node, error) {
	return []*livekit.Node{
		r.currentNode,
	}, nil
}

func (r *LocalRouter) StartParticipantSignal(ctx context.Context, roomKey livekit.RoomKey, pi ParticipantInit) (connectionID livekit.ConnectionID, reqSink MessageSink, resSource MessageSource, err error) {
	log.Printf("StartParticipantSignal start")

	if _, ok := r.routerCommunicators[roomKey]; !ok {
		db := r.mainDatabase
		r.routerCommunicators[roomKey] = p2p.NewRouterCommunicatorImpl(roomKey, db, r.writeFromP2P)
	}
	log.Printf("StartParticipantSignal progress")

	return r.StartParticipantSignalWithNodeID(ctx, roomKey, pi, livekit.NodeID(r.currentNode.Id))
}

func (r *LocalRouter) StartParticipantSignalWithNodeID(ctx context.Context, roomKey livekit.RoomKey, pi ParticipantInit, nodeID livekit.NodeID) (connectionID livekit.ConnectionID, reqSink MessageSink, resSource MessageSource, err error) {
	connectionID, reqSink, resSource, err = r.signalClient.StartParticipantSignal(ctx, roomKey, pi, livekit.NodeID(r.currentNode.Id))
	if err != nil {
		logger.Errorw("could not handle new participant", err,
			"room", roomKey,
			"participant", pi.Identity,
			"connectionID", connectionID,
		)
	}
	return
}

func (r *LocalRouter) WriteParticipantRTC(_ context.Context, roomKey livekit.RoomKey, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) error {
	r.lock.Lock()
	if r.rtcMessageChan.IsClosed() {
		// create a new one
		r.rtcMessageChan = NewMessageChannel(localRTCChannelSize)
	}
	r.lock.Unlock()
	msg.ParticipantKey = string(ParticipantKeyLegacy(roomKey, identity))
	msg.ParticipantKeyB62 = string(ParticipantKey(roomKey, identity))
	r.writeToP2P(roomKey, msg)
	return r.writeRTCMessage(r.rtcMessageChan, msg)
}

func (r *LocalRouter) writeFromP2P(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error {
	return r.WriteNodeRTC(ctx, r.currentNode.Id, msg)
}

func (r *LocalRouter) writeToP2P(roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) {
	if routerCommunicator, ok := r.routerCommunicators[roomKey]; !ok {
		log.Printf("writeToP2P no routerCommunicator %v", roomKey)
	} else {
		routerCommunicator.Publish(msg)
	}
}

func (r *LocalRouter) WriteRoomRTC(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error {
	msg.ParticipantKey = string(ParticipantKeyLegacy(roomKey, ""))
	msg.ParticipantKeyB62 = string(ParticipantKey(roomKey, ""))
	r.writeToP2P(roomKey, msg)
	return r.WriteNodeRTC(ctx, r.currentNode.Id, msg)
}

func (r *LocalRouter) WriteNodeRTC(_ context.Context, _ string, msg *livekit.RTCNodeMessage) error {
	r.lock.Lock()
	if r.rtcMessageChan.IsClosed() {
		// create a new one
		r.rtcMessageChan = NewMessageChannel(localRTCChannelSize)
	}
	r.lock.Unlock()
	return r.writeRTCMessage(r.rtcMessageChan, msg)
}

func (r *LocalRouter) writeRTCMessage(sink MessageSink, msg *livekit.RTCNodeMessage) error {
	defer sink.Close()
	msg.SenderTime = time.Now().Unix()
	return sink.WriteMessage(msg)
}

func (r *LocalRouter) OnNewParticipantRTC(callback NewParticipantCallback) {
	r.onNewParticipant = callback
}

func (r *LocalRouter) OnRTCMessage(callback RTCMessageCallback) {
	r.onRTCMessage = callback
}

func (r *LocalRouter) Start() error {
	if r.isStarted.Swap(true) {
		return nil
	}
	go r.statsWorker()
	// on local routers, Start doesn't do anything, websocket connections initiate the connections
	go r.rtcMessageWorker()
	return nil
}

func (r *LocalRouter) Drain() {
	r.currentNode.State = livekit.NodeState_SHUTTING_DOWN
}

func (r *LocalRouter) Stop() {
	r.rtcMessageChan.Close()
}

func (r *LocalRouter) GetRegion() string {
	return r.currentNode.Region
}

func (r *LocalRouter) statsWorker() {
	for {
		if !r.isStarted.Load() {
			return
		}
		// update every 10 seconds
		<-time.After(statsUpdateInterval)
		r.lock.Lock()
		r.currentNode.Stats.UpdatedAt = time.Now().Unix()
		r.lock.Unlock()
	}
}

func (r *LocalRouter) rtcMessageWorker() {
	// is a new channel available? if so swap to that one
	if !r.isStarted.Load() {
		return
	}

	// start a new worker after this finished
	defer func() {
		go r.rtcMessageWorker()
	}()

	r.lock.RLock()
	isClosed := r.rtcMessageChan.IsClosed()
	r.lock.RUnlock()
	if isClosed {
		// sleep and retry
		time.Sleep(time.Second)
	}

	r.lock.RLock()
	msgChan := r.rtcMessageChan.ReadChan()
	r.lock.RUnlock()
	// consume messages from
	for msg := range msgChan {
		if rtcMsg, ok := msg.(*livekit.RTCNodeMessage); ok {
			var roomKey livekit.RoomKey
			var identity livekit.ParticipantIdentity
			var err error
			if rtcMsg.ParticipantKeyB62 != "" {
				roomKey, identity, err = parseParticipantKey(livekit.ParticipantKey(rtcMsg.ParticipantKeyB62))
			}
			if err != nil {
				roomKey, identity, err = parseParticipantKeyLegacy(livekit.ParticipantKey(rtcMsg.ParticipantKey))
			}
			if err != nil {
				logger.Errorw("could not process RTC message", err)
				continue
			}
			if r.onRTCMessage != nil {
				r.onRTCMessage(context.Background(), roomKey, identity, rtcMsg)
			}
		}
	}
}

func (r *LocalRouter) getMessageChannel(target map[string]*MessageChannel, key string) *MessageChannel {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return target[key]
}

func (r *LocalRouter) getOrCreateMessageChannel(target map[string]*MessageChannel, key string) *MessageChannel {
	r.lock.Lock()
	defer r.lock.Unlock()
	mc := target[key]

	if mc != nil {
		return mc
	}

	mc = NewMessageChannel(DefaultMessageChannelSize)
	mc.OnClose(func() {
		r.lock.Lock()
		delete(target, key)
		r.lock.Unlock()
	})
	target[key] = mc

	return mc
}
