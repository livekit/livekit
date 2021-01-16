package routing

import (
	"sync"

	"github.com/livekit/livekit-server/proto/livekit"
)

// a router of messages
type LocalRouter struct {
	currentNode LocalNode
	lock        sync.RWMutex
	// channels for each participant
	requestChannels  map[string]*MessageChannel
	responseChannels map[string]*MessageChannel
	onNewParticipant ParticipantCallback
}

func NewLocalRouter(currentNode LocalNode) *LocalRouter {
	return &LocalRouter{
		currentNode:      currentNode,
		lock:             sync.RWMutex{},
		requestChannels:  make(map[string]*MessageChannel),
		responseChannels: make(map[string]*MessageChannel),
	}
}

func (r *LocalRouter) GetNodeIdForRoom(roomName string) (string, error) {
	return r.currentNode.Id, nil
}

func (r *LocalRouter) RegisterNode(node *livekit.Node) error {
	return nil
}

func (r *LocalRouter) GetNode(nodeId string) (*livekit.Node, error) {
	if nodeId == r.currentNode.Id {
		return r.currentNode, nil
	}
	return nil, ErrNodeNotFound
}

func (r *LocalRouter) StartParticipant(roomName, participantId, participantName, nodeId string) error {
	// treat it as a new participant connecting
	if r.onNewParticipant == nil {
		return ErrHandlerNotDefined
	}
	r.onNewParticipant(
		roomName,
		participantId,
		participantName,
		r.getOrCreateMessageChannel(r.requestChannels, participantId),
		r.getOrCreateMessageChannel(r.responseChannels, participantId),
	)
	return nil
}

func (r *LocalRouter) SetRTCNode(participantId, nodeId string) error {
	// nothing to be done
	return nil
}

// for a local router, sink and source are pointing to the same spot
func (r *LocalRouter) GetRequestSink(participantId string) MessageSink {
	return r.getOrCreateMessageChannel(r.requestChannels, participantId)
}

func (r *LocalRouter) GetResponseSource(participantId string) MessageSource {
	return r.getOrCreateMessageChannel(r.responseChannels, participantId)
}

func (r *LocalRouter) OnNewParticipant(callback ParticipantCallback) {
	r.onNewParticipant = callback
}

func (r *LocalRouter) Start() error {
	// on local routers, Start doesn't do anything, websocket connections initiate the connections
	return nil
}

func (r *LocalRouter) Stop() {
}

func (r *LocalRouter) getOrCreateMessageChannel(target map[string]*MessageChannel, participantId string) *MessageChannel {
	r.lock.RLock()
	mc := target[participantId]
	r.lock.RUnlock()

	if mc != nil {
		return mc
	}

	mc = NewMessageChannel()
	r.lock.Lock()
	target[participantId] = mc
	r.lock.Unlock()

	return mc
}
