package routing

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

// a router of messages on the same node, basic implementation for local testing
type LocalRouter struct {
	currentNode LocalNode
	lock        sync.Mutex
	// channels for each participant
	requestChannels  map[string]*MessageChannel
	responseChannels map[string]*MessageChannel
	isStarted        utils.AtomicFlag

	rtcMessageChan *MessageChannel

	onNewParticipant NewParticipantCallback
	onRTCMessage     RTCMessageCallback
}

func NewLocalRouter(currentNode LocalNode) *LocalRouter {
	return &LocalRouter{
		currentNode:      currentNode,
		requestChannels:  make(map[string]*MessageChannel),
		responseChannels: make(map[string]*MessageChannel),
		rtcMessageChan:   NewMessageChannel(),
	}
}

func (r *LocalRouter) GetNodeForRoom(roomName string) (string, error) {
	return r.currentNode.Id, nil
}

func (r *LocalRouter) SetNodeForRoom(roomName string, nodeId string) error {
	return nil
}

func (r *LocalRouter) ClearRoomState(roomName string) error {
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

func (r *LocalRouter) GetNode(nodeId string) (*livekit.Node, error) {
	if nodeId == r.currentNode.Id {
		return r.currentNode, nil
	}
	return nil, ErrNotFound
}

func (r *LocalRouter) ListNodes() ([]*livekit.Node, error) {
	return []*livekit.Node{
		r.currentNode,
	}, nil
}

func (r *LocalRouter) StartParticipantSignal(roomName, identity, metadata string, reconnect bool) (connectionId string, reqSink MessageSink, resSource MessageSource, err error) {
	// treat it as a new participant connecting
	if r.onNewParticipant == nil {
		err = ErrHandlerNotDefined
		return
	}

	// index channels by roomName | identity
	key := participantKey(roomName, identity)
	reqChan := r.getOrCreateMessageChannel(r.requestChannels, key)
	resChan := r.getOrCreateMessageChannel(r.responseChannels, key)

	r.onNewParticipant(
		roomName,
		identity,
		metadata,
		reconnect,
		// request source
		reqChan,
		// response sink
		resChan,
	)
	return identity, reqChan, resChan, nil
}

func (r *LocalRouter) CreateRTCSink(roomName, identity string) (MessageSink, error) {
	if r.rtcMessageChan.isClosed.Get() {
		// create a new one
		r.rtcMessageChan = NewMessageChannel()
	}
	return r.rtcMessageChan, nil
}

func (r *LocalRouter) OnNewParticipantRTC(callback NewParticipantCallback) {
	r.onNewParticipant = callback
}

func (r *LocalRouter) OnRTCMessage(callback RTCMessageCallback) {
	r.onRTCMessage = callback
}

func (r *LocalRouter) Start() error {
	if !r.isStarted.TrySet(true) {
		return nil
	}
	go r.statsWorker()
	// on local routers, Start doesn't do anything, websocket connections initiate the connections
	go r.rtcMessageWorker()
	return nil
}

func (r *LocalRouter) Stop() {

	r.rtcMessageChan.Close()
}

func (r *LocalRouter) statsWorker() {
	for {
		if !r.isStarted.Get() {
			return
		}
		// update every 10 seconds
		<-time.After(statsUpdateInterval)
		r.currentNode.Stats.UpdatedAt = time.Now().Unix()
	}
}

func (r *LocalRouter) rtcMessageWorker() {
	// is a new channel available? if so swap to that one
	if !r.isStarted.Get() {
		return
	}

	// start a new worker after this finished
	defer func() {
		go r.rtcMessageWorker()
	}()

	if r.rtcMessageChan.isClosed.Get() {
		// sleep and retry
		time.Sleep(time.Second)
	}

	// consume messages from
	for msg := range r.rtcMessageChan.ReadChan() {
		if rtcMsg, ok := msg.(*livekit.RTCNodeMessage); ok {
			room, identity, err := parseParticipantKey(rtcMsg.ParticipantKey)
			if err != nil {
				logger.Errorw("could not process RTC message", "error", err)
				continue
			}
			if r.onRTCMessage != nil {
				r.onRTCMessage(room, identity, rtcMsg)
			}
		}
	}
}

func (r *LocalRouter) getOrCreateMessageChannel(target map[string]*MessageChannel, key string) *MessageChannel {
	r.lock.Lock()
	defer r.lock.Unlock()
	mc := target[key]

	if mc != nil {
		return mc
	}

	mc = NewMessageChannel()
	mc.OnClose(func() {
		r.lock.Lock()
		delete(target, key)
		r.lock.Unlock()
	})
	target[key] = mc

	return mc
}
