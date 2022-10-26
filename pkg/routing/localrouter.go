package routing

import (
	"context"
	"runtime"
	"sync"
	"time"

	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

// aggregated channel for all participants
const localRTCChannelSize = 10000

// a router of messages on the same node, basic implementation for local testing
type LocalRouter struct {
	currentNode LocalNode
	lock        sync.RWMutex
	// channels for each participant
	requestChannels  map[string]*MessageChannel
	responseChannels map[string]*MessageChannel
	isStarted        atomic.Bool

	rtcMessageChan *MessageChannel

	onNewParticipant NewParticipantCallback
	onRTCMessage     RTCMessageCallback
}

func NewLocalRouter(currentNode LocalNode) *LocalRouter {
	return &LocalRouter{
		currentNode:      currentNode,
		requestChannels:  make(map[string]*MessageChannel),
		responseChannels: make(map[string]*MessageChannel),
		rtcMessageChan:   NewMessageChannel(localRTCChannelSize),
	}
}

func (r *LocalRouter) GetNodeForRoom(_ context.Context, _ livekit.RoomName) (*livekit.Node, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	node := proto.Clone((*livekit.Node)(r.currentNode)).(*livekit.Node)
	return node, nil
}

func (r *LocalRouter) SetNodeForRoom(_ context.Context, _ livekit.RoomName, _ livekit.NodeID) error {
	return nil
}

func (r *LocalRouter) ClearRoomState(_ context.Context, _ livekit.RoomName) error {
	// do nothing
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

func (r *LocalRouter) StartParticipantSignal(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit) (connectionID livekit.ConnectionID, reqSink MessageSink, resSource MessageSource, err error) {
	// treat it as a new participant connecting
	if r.onNewParticipant == nil {
		err = ErrHandlerNotDefined
		return
	}

	// create a new connection id
	connectionID = livekit.ConnectionID(utils.NewGuid("CO_"))
	// index channels by roomName | identity
	key := participantKey(roomName, pi.Identity)
	key = key + "|" + livekit.ParticipantKey(connectionID)

	// close older channels if one already exists
	reqChan := r.getMessageChannel(r.requestChannels, string(key))
	if reqChan != nil {
		reqChan.Close()
	}
	resChan := r.getMessageChannel(r.responseChannels, string(key))
	if resChan != nil {
		resChan.Close()
	}
	reqChan = r.getOrCreateMessageChannel(r.requestChannels, string(key))
	resChan = r.getOrCreateMessageChannel(r.responseChannels, string(key))

	go func() {
		err := r.onNewParticipant(
			ctx,
			roomName,
			pi,
			// request source
			reqChan,
			// response sink
			resChan,
		)
		if err != nil {
			reqChan.Close()
			resChan.Close()
			logger.Errorw("could not handle new participant", err,
				"room", roomName,
				"participant", pi.Identity,
			)
		}
	}()
	return connectionID, reqChan, resChan, nil
}

func (r *LocalRouter) WriteParticipantRTC(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) error {
	if r.rtcMessageChan.IsClosed() {
		// create a new one
		r.rtcMessageChan = NewMessageChannel(localRTCChannelSize)
	}
	msg.ParticipantKey = string(participantKey(roomName, identity))
	return r.writeRTCMessage(r.rtcMessageChan, msg)
}

func (r *LocalRouter) WriteRoomRTC(ctx context.Context, roomName livekit.RoomName, msg *livekit.RTCNodeMessage) error {
	msg.ParticipantKey = string(participantKey(roomName, ""))
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
	go r.memStatsWorker()
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

func (r *LocalRouter) memStatsWorker() {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	for {
		<-ticker.C

		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		logger.Infow("memstats",
			"mallocs", m.Mallocs, "frees", m.Frees, "m-f", m.Mallocs-m.Frees,
			"hinuse", m.HeapInuse, "halloc", m.HeapAlloc, "frag", m.HeapInuse-m.HeapAlloc,
		)

		runtime.GC()
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
			room, identity, err := parseParticipantKey(livekit.ParticipantKey(rtcMsg.ParticipantKey))
			if err != nil {
				logger.Errorw("could not process RTC message", err)
				continue
			}
			if r.onRTCMessage != nil {
				r.onRTCMessage(context.Background(), room, identity, rtcMsg)
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
