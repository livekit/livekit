package routing

import (
	"context"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	// expire participant mappings after a day
	participantMappingTTL = 24 * time.Hour
	statsUpdateInterval   = 10 * time.Second
)

// RedisRouter uses Redis pub/sub to route signaling messages across different nodes
// It relies on the RTC node to be the primary driver of the participant connection.
// Because
type RedisRouter struct {
	LocalRouter
	rc   *redis.Client
	cr   *utils.CachedRedis
	ctx  context.Context
	once sync.Once

	// map of participantKey => RTCNodeSink
	rtcSinks map[string]*RTCNodeSink
	// map of connectionId => SignalNodeSink
	signalSinks map[string]*SignalNodeSink
	cancel      func()
}

func NewRedisRouter(currentNode LocalNode, rc *redis.Client) *RedisRouter {
	rr := &RedisRouter{
		LocalRouter: *NewLocalRouter(currentNode),
		rc:          rc,
		once:        sync.Once{},
		rtcSinks:    make(map[string]*RTCNodeSink),
		signalSinks: make(map[string]*SignalNodeSink),
	}
	rr.ctx, rr.cancel = context.WithCancel(context.Background())
	rr.cr = utils.NewCachedRedis(rr.ctx, rr.rc)
	return rr
}

func (r *RedisRouter) RegisterNode() error {
	data, err := proto.Marshal((*livekit.Node)(r.currentNode))
	if err != nil {
		return err
	}
	r.cr.ExpireHash(NodesKey, r.currentNode.Id)
	if err := r.rc.HSet(r.ctx, NodesKey, r.currentNode.Id, data).Err(); err != nil {
		return errors.Wrap(err, "could not register node")
	}
	return nil
}

func (r *RedisRouter) UnregisterNode() error {
	// could be called after Stop(), so we'd want to use an unrelated context
	return r.rc.HDel(context.Background(), NodesKey, r.currentNode.Id).Err()
}

func (r *RedisRouter) GetNodeForRoom(roomName string) (string, error) {
	val, err := r.cr.CachedHGet(NodeRoomKey, roomName)
	if err != nil {
		err = errors.Wrap(err, "could not get node for room")
	}
	return val, err
}

func (r *RedisRouter) SetNodeForRoom(roomName string, nodeId string) error {
	return r.rc.HSet(r.ctx, NodeRoomKey, roomName, nodeId).Err()
}

func (r *RedisRouter) ClearRoomState(roomName string) error {
	if err := r.rc.HDel(r.ctx, NodeRoomKey, roomName).Err(); err != nil {
		return errors.Wrap(err, "could not clear room state")
	}
	return nil
}

func (r *RedisRouter) GetNode(nodeId string) (*livekit.Node, error) {
	data, err := r.cr.CachedHGet(NodesKey, nodeId)
	if err != nil {
		return nil, err
	}
	n := livekit.Node{}
	if err = proto.Unmarshal([]byte(data), &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func (r *RedisRouter) ListNodes() ([]*livekit.Node, error) {
	items, err := r.rc.HVals(r.ctx, NodesKey).Result()
	if err != nil {
		return nil, errors.Wrap(err, "could not list nodes")
	}
	nodes := make([]*livekit.Node, 0, len(items))
	for _, item := range items {
		n := livekit.Node{}
		if err := proto.Unmarshal([]byte(item), &n); err != nil {
			return nil, err
		}
		nodes = append(nodes, &n)
	}
	return nodes, nil
}

// signal connection sets up paths to the RTC node, and starts to route messages to that message queue
func (r *RedisRouter) StartParticipantSignal(roomName, identity string) (reqSink MessageSink, resSource MessageSource, err error) {
	// find the node where the room is hosted at
	rtcNode, err := r.GetNodeForRoom(roomName)
	if err != nil {
		return
	}

	// create a new connection id
	connectionId := utils.NewGuid("CO_")
	pKey := participantKey(roomName, identity)

	// map signal & rtc nodes
	if err = r.setParticipantSignalNode(connectionId, r.currentNode.Id); err != nil {
		return
	}

	sink := r.getOrCreateRTCSink(rtcNode, pKey)

	// sends a message to start session
	err = sink.WriteMessage(&livekit.StartSession{
		RoomName: roomName,
		Identity: identity,
		// connection id is to allow the RTC node to identify where to route the message back to
		ConnectionId: connectionId,
	})
	if err != nil {
		return
	}

	// index by connectionId, since there may be multiple connections for the participant
	resChan := r.getOrCreateMessageChannel(r.responseChannels, connectionId)
	return sink, resChan, nil
}

func (r *RedisRouter) startParticipantRTC(ss *livekit.StartSession, participantKey string) error {
	// find the node where the room is hosted at
	rtcNode, err := r.GetNodeForRoom(ss.RoomName)
	if err != nil {
		return err
	}

	if rtcNode != r.currentNode.Id {
		logger.Errorw("called participant on incorrect node",
			"rtcNode", rtcNode, "currentNode", r.currentNode.Id)
		return ErrIncorrectRTCNode
	}

	if err := r.setParticipantRTCNode(participantKey, rtcNode); err != nil {
		return err
	}

	// find signal node to send responses back
	signalNode, err := r.getParticipantSignalNode(ss.ConnectionId)
	if err != nil {
		return err
	}

	// treat it as a new participant connecting
	if r.onNewParticipant == nil {
		return ErrHandlerNotDefined
	}

	reqChan := r.getOrCreateMessageChannel(r.requestChannels, participantKey)
	resSink := r.getOrCreateSignalSink(signalNode, ss.ConnectionId)
	r.onNewParticipant(
		ss.RoomName,
		ss.Identity,
		reqChan,
		resSink,
	)
	return nil
}

func (r *RedisRouter) Start() error {
	r.once.Do(func() {
		go r.statsWorker()
		go r.rtcWorker()
		go r.signalWorker()
	})
	return nil
}

func (r *RedisRouter) Stop() {
	r.cancel()
}

func (r *RedisRouter) setParticipantRTCNode(participantKey, nodeId string) error {
	r.cr.Expire(participantRTCKey(participantKey))
	err := r.rc.Set(r.ctx, participantRTCKey(participantKey), nodeId, participantMappingTTL).Err()
	if err != nil {
		err = errors.Wrap(err, "could not set rtc node")
	}
	return err
}

func (r *RedisRouter) setParticipantSignalNode(connectionId, nodeId string) error {
	r.cr.Expire(participantSignalKey(connectionId))
	if err := r.rc.Set(r.ctx, participantSignalKey(connectionId), nodeId, participantMappingTTL).Err(); err != nil {
		return errors.Wrap(err, "could not set signal node")
	}
	return nil
}

func (r *RedisRouter) getOrCreateRTCSink(nodeId string, participantKey string) *RTCNodeSink {
	r.lock.Lock()
	defer r.lock.Unlock()
	sink := r.rtcSinks[participantKey]

	if sink != nil {
		return sink
	}

	sink = NewRTCNodeSink(r.rc, nodeId, participantKey)
	sink.OnClose(func() {
		r.lock.Lock()
		delete(r.rtcSinks, participantKey)
		r.lock.Unlock()
	})
	r.rtcSinks[participantKey] = sink
	return sink
}

func (r *RedisRouter) getOrCreateSignalSink(nodeId string, connectionId string) *SignalNodeSink {
	r.lock.Lock()
	defer r.lock.Unlock()
	sink := r.signalSinks[connectionId]

	if sink != nil {
		return sink
	}

	sink = NewSignalNodeSink(r.rc, nodeId, connectionId)
	sink.OnClose(func() {
		r.lock.Lock()
		delete(r.signalSinks, connectionId)
		r.lock.Unlock()
	})
	r.signalSinks[connectionId] = sink
	return sink
}

func (r *RedisRouter) getParticipantRTCNode(participantKey string) (string, error) {
	return r.cr.CachedGet(participantRTCKey(participantKey))
}

func (r *RedisRouter) getParticipantSignalNode(connectionId string) (nodeId string, err error) {
	return r.cr.CachedGet(participantSignalKey(connectionId))
}

// update node stats and cleanup
func (r *RedisRouter) statsWorker() {
	for r.ctx.Err() == nil {
		// update every 10 seconds
		<-time.After(statsUpdateInterval)
		r.currentNode.Stats.UpdatedAt = time.Now().Unix()
		if err := r.RegisterNode(); err != nil {
			logger.Errorw("could not update node", "error", err)
		}
	}
}

// worker that consumes signal channel and processes
func (r *RedisRouter) signalWorker() {
	sub := r.rc.Subscribe(redisCtx, signalNodeChannel(r.currentNode.Id))
	defer func() {
		logger.Debugw("finishing redis signalWorker", "node", r.currentNode.Id)
	}()
	logger.Debugw("starting redis signalWorker", "node", r.currentNode.Id)
	for r.ctx.Err() == nil {
		obj, err := sub.Receive(r.ctx)
		if err != nil {
			logger.Warnw("error receiving redis message", "error", err)
			// TODO: retry? ignore? at a minimum need to sleep here to retry
			time.Sleep(time.Second)
			continue
		}
		if obj == nil {
			return
		}

		msg, ok := obj.(*redis.Message)
		if !ok {
			continue
		}

		rm := livekit.SignalNodeMessage{}
		err = proto.Unmarshal([]byte(msg.Payload), &rm)
		connectionId := rm.ConnectionId

		switch rmb := rm.Message.(type) {

		case *livekit.SignalNodeMessage_Response:
			// in the event the current node is an Signal node, push to response channels
			resSink := r.getOrCreateMessageChannel(r.responseChannels, connectionId)
			err = resSink.WriteMessage(rmb.Response)
			if err != nil {
				logger.Errorw("could not write to response channel",
					"connectionId", connectionId,
					"error", err)
			}

		case *livekit.SignalNodeMessage_EndSession:
			signalNode, err := r.getParticipantSignalNode(connectionId)
			if err != nil {
				logger.Errorw("could not get participant RTC node",
					"error", err)
				continue
			}
			// EndSession can only be initiated on an RTC node, is handled on the signal node
			if signalNode == r.currentNode.Id {
				resSink := r.getOrCreateMessageChannel(r.responseChannels, connectionId)
				resSink.Close()
			}
		}
	}
}

// worker that consumes RTC channel and processes
func (r *RedisRouter) rtcWorker() {
	sub := r.rc.Subscribe(redisCtx, rtcNodeChannel(r.currentNode.Id))

	defer func() {
		logger.Debugw("finishing redis rtcWorker", "node", r.currentNode.Id)
	}()
	logger.Debugw("starting redis rtcWorker", "node", r.currentNode.Id)
	for r.ctx.Err() == nil {
		obj, err := sub.Receive(r.ctx)
		if err != nil {
			logger.Warnw("error receiving redis message", "error", err)
			// TODO: retry? ignore? at a minimum need to sleep here to retry
			time.Sleep(time.Second)
			continue
		}
		if obj == nil {
			return
		}

		msg, ok := obj.(*redis.Message)
		if !ok {
			continue
		}

		rm := livekit.RTCNodeMessage{}
		err = proto.Unmarshal([]byte(msg.Payload), &rm)
		pKey := rm.ParticipantKey

		switch rmb := rm.Message.(type) {
		case *livekit.RTCNodeMessage_StartSession:
			logger.Debugw("received router startSession", "node", r.currentNode.Id,
				"participant", pKey)
			// RTC session should start on this node
			err = r.startParticipantRTC(rmb.StartSession, pKey)
			if err != nil {
				logger.Errorw("could not start participant", "error", err)
			}

		case *livekit.RTCNodeMessage_Request:
			// in the event the current node is an RTC node, push to request channels
			reqSink := r.getOrCreateMessageChannel(r.requestChannels, pKey)
			err = reqSink.WriteMessage(rmb.Request)
			if err != nil {
				logger.Errorw("could not write to request channel",
					"participant", pKey,
					"error", err)
			}
		}
	}
}
