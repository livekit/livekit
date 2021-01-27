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
	useLocal bool
	rc       *redis.Client
	cr       *utils.CachedRedis
	ctx      context.Context
	once     sync.Once

	redisSinks map[string]*RedisSink
	cancel     func()
}

func NewRedisRouter(currentNode LocalNode, rc *redis.Client, useLocal bool) *RedisRouter {
	rr := &RedisRouter{
		LocalRouter: *NewLocalRouter(currentNode),
		useLocal:    useLocal,
		rc:          rc,
		once:        sync.Once{},
		redisSinks:  make(map[string]*RedisSink),
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

func (r *RedisRouter) SetParticipantRTCNode(participantId, nodeId string) error {
	r.cr.Expire(participantRTCKey(participantId))
	err := r.rc.Set(r.ctx, participantRTCKey(participantId), nodeId, participantMappingTTL).Err()
	if err != nil {
		err = errors.Wrap(err, "could not set rtc node")
	}
	return err
}

// for a local router, sink and source are pointing to the same spot
func (r *RedisRouter) GetRequestSink(participantId string) (MessageSink, error) {
	// request should go to RTC node
	rtcNode, err := r.getParticipantRTCNode(participantId)
	if err != nil {
		return nil, err
	}

	if rtcNode == r.currentNode.Id && r.useLocal {
		return r.LocalRouter.GetRequestSink(participantId)
	}

	sink := r.getOrCreateRedisSink(rtcNode, participantId)
	return sink, nil
}

func (r *RedisRouter) GetResponseSource(participantId string) (MessageSource, error) {
	// request should go to RTC node
	rtcNode, err := r.getParticipantRTCNode(participantId)
	if err != nil {
		return nil, err
	}

	if rtcNode == r.currentNode.Id && r.useLocal {
		return r.LocalRouter.GetResponseSource(participantId)
	}

	// a message channel that we'll send data into
	source := r.getOrCreateMessageChannel(r.responseChannels, participantId)
	return source, nil
}

// StartParticipant always called on the signal node
func (r *RedisRouter) StartParticipant(roomName, participantId, participantName string) error {
	// find the node where the room is hosted at
	rtcNode, err := r.GetNodeForRoom(roomName)
	if err != nil {
		return err
	}

	if r.useLocal && rtcNode == r.currentNode.Id {
		return r.LocalRouter.StartParticipant(roomName, participantId, participantId)
	}

	err = r.setParticipantSignalNode(participantId, r.currentNode.Id)
	if err != nil {
		return err
	}

	// find signal node to send responses back
	signalNode, err := r.getParticipantSignalNode(participantId)
	if err != nil {
		return err
	}

	// treat it as a new participant connecting
	if r.onNewParticipant == nil {
		return ErrHandlerNotDefined
	}

	resSink := r.getOrCreateRedisSink(signalNode, participantId)
	r.onNewParticipant(
		roomName,
		participantId,
		participantName,
		r.getOrCreateMessageChannel(r.requestChannels, participantId),
		resSink,
	)
	return nil
}

func (r *RedisRouter) Start() error {
	r.once.Do(func() {
		go r.statsWorker()
		go r.subscribeWorker()
	})
	return nil
}

func (r *RedisRouter) Stop() {
	r.cancel()
}

func (r *RedisRouter) setParticipantSignalNode(participantId, nodeId string) error {
	r.cr.Expire(participantSignalKey(participantId))
	if err := r.rc.Set(r.ctx, participantSignalKey(participantId), nodeId, participantMappingTTL).Err(); err != nil {
		return errors.Wrap(err, "could not set signal node")
	}
	return nil
}

func (r *RedisRouter) getOrCreateRedisSink(nodeId string, participantId string) *RedisSink {
	r.lock.RLock()
	sink := r.redisSinks[participantId]
	r.lock.RUnlock()

	if sink != nil {
		return sink
	}

	sink = NewRedisSink(r.rc, nodeId, participantId)
	sink.OnClose(func() {
		r.lock.Lock()
		delete(r.redisSinks, participantId)
		r.lock.Unlock()
	})
	r.lock.Lock()
	r.redisSinks[participantId] = sink
	r.lock.Unlock()
	return sink
}

func (r *RedisRouter) getParticipantRTCNode(participantId string) (string, error) {
	return r.cr.CachedGet(participantRTCKey(participantId))
}

func (r *RedisRouter) getParticipantSignalNode(participantId string) (nodeId string, err error) {
	return r.cr.CachedGet(participantSignalKey(participantId))
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

func (r *RedisRouter) subscribeWorker() {
	sub := r.rc.Subscribe(redisCtx, nodeChannel(r.currentNode.Id))

	defer func() {
		logger.Debugw("finishing redis subscribeWorker", "node", r.currentNode.Id)
	}()
	logger.Debugw("starting redis subscribeWorker", "node", r.currentNode.Id)
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

		rm := livekit.RouterMessage{}
		err = proto.Unmarshal([]byte(msg.Payload), &rm)
		pId := rm.ParticipantId

		switch rmb := rm.Message.(type) {
		case *livekit.RouterMessage_StartSession:
			// RTC session should start on this node
			err = r.StartParticipant(rmb.StartSession.RoomName, pId, rmb.StartSession.ParticipantName)
			if err != nil {
				logger.Errorw("could not start participant", "error", err)
			}

		case *livekit.RouterMessage_Request:
			// in the event the current node is an RTC node, push to request channels
			reqSink := r.getOrCreateMessageChannel(r.requestChannels, pId)
			err = reqSink.WriteMessage(rmb.Request)
			if err != nil {
				logger.Errorw("could not write to request channel",
					"participant", pId,
					"error", err)
			}

		case *livekit.RouterMessage_Response:
			// in the event the current node is an Signal node, push to response channels
			resSink := r.getOrCreateMessageChannel(r.responseChannels, pId)
			err = resSink.WriteMessage(rmb.Response)
			if err != nil {
				logger.Errorw("could not write to response channel",
					"participant", pId,
					"error", err)
			}

		case *livekit.RouterMessage_EndSession:
			signalNode, err := r.getParticipantRTCNode(pId)
			if err != nil {
				logger.Errorw("could not get participant RTC node",
					"error", err)
				continue
			}
			// EndSession can only be initiated on an RTC node, is handled on the signal node
			if signalNode == r.currentNode.Id {
				resSink := r.getOrCreateMessageChannel(r.responseChannels, pId)
				resSink.Close()
			}
		}
	}
}
