package routing

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/proto/livekit"
)

// a router of messages on the same node, basic implementation for local testing
type LocalRouter struct {
	currentNode LocalNode
	lock        sync.Mutex
	// channels for each participant
	requestChannels  map[string]*MessageChannel
	responseChannels map[string]*MessageChannel
	onNewParticipant ParticipantCallback
}

func NewLocalRouter(currentNode LocalNode) *LocalRouter {
	return &LocalRouter{
		currentNode:      currentNode,
		requestChannels:  make(map[string]*MessageChannel),
		responseChannels: make(map[string]*MessageChannel),
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

func (r *LocalRouter) StartParticipantSignal(roomName, identity string) (reqSink MessageSink, resSource MessageSource, err error) {
	// treat it as a new participant connecting
	if r.onNewParticipant == nil {
		return nil, nil, ErrHandlerNotDefined
	}

	// index channels by roomName | identity
	key := participantKey(roomName, identity)
	reqChan := r.getOrCreateMessageChannel(r.requestChannels, key)
	resChan := r.getOrCreateMessageChannel(r.responseChannels, key)

	r.onNewParticipant(
		roomName,
		identity,
		// request source
		reqChan,
		// response sink
		resChan,
	)
	return reqChan, resChan, nil
}

func (r *LocalRouter) OnNewParticipantRTC(callback ParticipantCallback) {
	r.onNewParticipant = callback
}

func (r *LocalRouter) Start() error {
	go r.statsWorker()
	// on local routers, Start doesn't do anything, websocket connections initiate the connections
	return nil
}

func (r *LocalRouter) Stop() {
}

func (r *LocalRouter) statsWorker() {
	for {
		// update every 10 seconds
		<-time.After(statsUpdateInterval)
		r.currentNode.Stats.UpdatedAt = time.Now().Unix()
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
