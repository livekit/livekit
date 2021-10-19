package routing

import (
	"context"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/utils/stats"
)

const (
	// expire participant mappings after a day
	participantMappingTTL = 24 * time.Hour
	statsUpdateInterval   = 2 * time.Second
)

// RedisRouter uses Redis pub/sub to route signaling messages across different nodes
// It relies on the RTC node to be the primary driver of the participant connection.
// Because
type RedisRouter struct {
	LocalRouter

	rc        *redis.Client
	ctx       context.Context
	isStarted utils.AtomicFlag

	pubsub *redis.PubSub
	cancel func()
}

func NewRedisRouter(currentNode LocalNode, rc *redis.Client) *RedisRouter {
	rr := &RedisRouter{
		LocalRouter: *NewLocalRouter(currentNode),
		rc:          rc,
	}
	rr.ctx, rr.cancel = context.WithCancel(context.Background())
	return rr
}

func (r *RedisRouter) RegisterNode() error {
	data, err := proto.Marshal((*livekit.Node)(r.currentNode))
	if err != nil {
		return err
	}
	if err := r.rc.HSet(r.ctx, NodesKey, r.currentNode.Id, data).Err(); err != nil {
		return errors.Wrap(err, "could not register node")
	}
	return nil
}

func (r *RedisRouter) UnregisterNode() error {
	// could be called after Stop(), so we'd want to use an unrelated context
	return r.rc.HDel(context.Background(), NodesKey, r.currentNode.Id).Err()
}

func (r *RedisRouter) RemoveDeadNodes() error {
	nodes, err := r.ListNodes()
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if !selector.IsAvailable(n) {
			if err := r.rc.HDel(context.Background(), NodesKey, n.Id).Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *RedisRouter) GetNodeForRoom(ctx context.Context, roomName string) (*livekit.Node, error) {
	nodeId, err := r.rc.HGet(r.ctx, NodeRoomKey, roomName).Result()
	if err == redis.Nil {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, errors.Wrap(err, "could not get node for room")
	}

	return r.GetNode(nodeId)
}

func (r *RedisRouter) SetNodeForRoom(ctx context.Context, roomName, nodeId string) error {
	return r.rc.HSet(r.ctx, NodeRoomKey, roomName, nodeId).Err()
}

func (r *RedisRouter) ClearRoomState(ctx context.Context, roomName string) error {
	if err := r.rc.HDel(r.ctx, NodeRoomKey, roomName).Err(); err != nil {
		return errors.Wrap(err, "could not clear room state")
	}
	return nil
}

func (r *RedisRouter) GetNode(nodeId string) (*livekit.Node, error) {
	data, err := r.rc.HGet(r.ctx, NodesKey, nodeId).Result()
	if err == redis.Nil {
		return nil, ErrNotFound
	} else if err != nil {
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

// StartParticipantSignal signal connection sets up paths to the RTC node, and starts to route messages to that message queue
func (r *RedisRouter) StartParticipantSignal(ctx context.Context, roomName string, pi ParticipantInit) (connectionId string, reqSink MessageSink, resSource MessageSource, err error) {
	// find the node where the room is hosted at
	rtcNode, err := r.GetNodeForRoom(ctx, roomName)
	if err != nil {
		return
	}

	// create a new connection id
	connectionId = utils.NewGuid("CO_")
	pKey := participantKey(roomName, pi.Identity)

	// map signal & rtc nodes
	if err = r.setParticipantSignalNode(connectionId, r.currentNode.Id); err != nil {
		return
	}

	sink := NewRTCNodeSink(r.rc, rtcNode.Id, pKey)

	// sends a message to start session
	err = sink.WriteMessage(&livekit.StartSession{
		RoomName: roomName,
		Identity: pi.Identity,
		Metadata: pi.Metadata,
		// connection id is to allow the RTC node to identify where to route the message back to
		ConnectionId:  connectionId,
		Reconnect:     pi.Reconnect,
		Permission:    pi.Permission,
		AutoSubscribe: pi.AutoSubscribe,
		Hidden:        pi.Hidden,
		Client:        pi.Client,
	})
	if err != nil {
		return
	}

	// index by connectionId, since there may be multiple connections for the participant
	resChan := r.getOrCreateMessageChannel(r.responseChannels, connectionId)
	return connectionId, sink, resChan, nil
}

func (r *RedisRouter) WriteRTCMessage(ctx context.Context, roomName, identity string, msg *livekit.RTCNodeMessage) error {
	pkey := participantKey(roomName, identity)
	rtcNode, err := r.getParticipantRTCNode(pkey)
	if err != nil {
		return err
	}

	rtcSink := NewRTCNodeSink(r.rc, rtcNode, pkey)
	return r.writeRTCMessage(roomName, identity, msg, rtcSink)
}

func (r *RedisRouter) startParticipantRTC(ss *livekit.StartSession, participantKey string) error {
	// find the node where the room is hosted at
	rtcNode, err := r.GetNodeForRoom(r.ctx, ss.RoomName)
	if err != nil {
		return err
	}

	if rtcNode.Id != r.currentNode.Id {
		err = ErrIncorrectRTCNode
		logger.Errorw("called participant on incorrect node", err,
			"rtcNode", rtcNode, "nodeID", r.currentNode.Id)
		return err
	}

	if err := r.setParticipantRTCNode(participantKey, rtcNode.Id); err != nil {
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

	if !ss.Reconnect {
		// when it's not reconnecting, we do not want to re-use the same response sink
		// the previous rtc worker thread is still consuming off of it.
		// we'll want to sever the connection and switch to the new one
		r.lock.RLock()
		requestChan, ok := r.requestChannels[participantKey]
		r.lock.RUnlock()
		if ok {
			requestChan.Close()
		}
	}

	pi := ParticipantInit{
		Identity:      ss.Identity,
		Metadata:      ss.Metadata,
		Reconnect:     ss.Reconnect,
		Permission:    ss.Permission,
		Client:        ss.Client,
		AutoSubscribe: ss.AutoSubscribe,
		Hidden:        ss.Hidden,
	}

	reqChan := r.getOrCreateMessageChannel(r.requestChannels, participantKey)
	resSink := NewSignalNodeSink(r.rc, signalNode, ss.ConnectionId)
	r.onNewParticipant(
		r.ctx,
		ss.RoomName,
		pi,
		reqChan,
		resSink,
	)
	return nil
}

func (r *RedisRouter) Start() error {
	if !r.isStarted.TrySet(true) {
		return nil
	}

	workerStarted := make(chan struct{})
	go r.statsWorker()
	go r.redisWorker(workerStarted)

	// wait until worker is running
	select {
	case <-workerStarted:
		return nil
	case <-time.After(3 * time.Second):
		return errors.New("Unable to start redis router")
	}
}

func (r *RedisRouter) Drain() {
	r.currentNode.State = livekit.NodeState_SHUTTING_DOWN
	r.RegisterNode()
}

func (r *RedisRouter) Stop() {
	if !r.isStarted.TrySet(false) {
		return
	}
	logger.Debugw("stopping RedisRouter")
	_ = r.pubsub.Close()
	_ = r.UnregisterNode()
	r.cancel()
}

func (r *RedisRouter) setParticipantRTCNode(participantKey, nodeId string) error {
	err := r.rc.Set(r.ctx, participantRTCKey(participantKey), nodeId, participantMappingTTL).Err()
	if err != nil {
		err = errors.Wrap(err, "could not set rtc node")
	}
	return err
}

func (r *RedisRouter) setParticipantSignalNode(connectionId, nodeId string) error {
	if err := r.rc.Set(r.ctx, participantSignalKey(connectionId), nodeId, participantMappingTTL).Err(); err != nil {
		return errors.Wrap(err, "could not set signal node")
	}
	return nil
}

func (r *RedisRouter) getParticipantRTCNode(participantKey string) (string, error) {
	val, err := r.rc.Get(r.ctx, participantRTCKey(participantKey)).Result()
	if err == redis.Nil {
		err = ErrNodeNotFound
	}
	return val, err
}

func (r *RedisRouter) getParticipantSignalNode(connectionId string) (nodeId string, err error) {
	val, err := r.rc.Get(r.ctx, participantSignalKey(connectionId)).Result()
	if err == redis.Nil {
		err = ErrNodeNotFound
	}
	return val, err
}

// update node stats and cleanup
func (r *RedisRouter) statsWorker() {
	for r.ctx.Err() == nil {
		// update periodically seconds
		select {
		case <-time.After(statsUpdateInterval):
			if err := stats.UpdateCurrentNodeStats(r.currentNode.Stats); err != nil {
				logger.Errorw("could not update node stats", err, "nodeID", r.currentNode.Id)
			}
			if err := r.RegisterNode(); err != nil {
				logger.Errorw("could not update node", err, "nodeID", r.currentNode.Id)
			}
		case <-r.ctx.Done():
			return
		}
	}
}

// worker that consumes redis messages intended for this node
func (r *RedisRouter) redisWorker(startedChan chan struct{}) {
	defer func() {
		logger.Debugw("finishing redisWorker", "nodeID", r.currentNode.Id)
	}()
	logger.Debugw("starting redisWorker", "nodeID", r.currentNode.Id)

	sigChannel := signalNodeChannel(r.currentNode.Id)
	rtcChannel := rtcNodeChannel(r.currentNode.Id)
	r.pubsub = r.rc.Subscribe(r.ctx, sigChannel, rtcChannel)

	close(startedChan)
	for msg := range r.pubsub.Channel() {
		if msg == nil {
			return
		}

		if msg.Channel == sigChannel {
			sm := livekit.SignalNodeMessage{}
			if err := proto.Unmarshal([]byte(msg.Payload), &sm); err != nil {
				logger.Errorw("could not unmarshal signal message on sigchan", err)
				stats.PromMessageCounter.WithLabelValues("signal", "failure").Add(1)
				continue
			}
			if err := r.handleSignalMessage(&sm); err != nil {
				logger.Errorw("error processing signal message", err)
				stats.PromMessageCounter.WithLabelValues("signal", "failure").Add(1)
				continue
			}
			stats.PromMessageCounter.WithLabelValues("signal", "success").Add(1)
		} else if msg.Channel == rtcChannel {
			rm := livekit.RTCNodeMessage{}
			if err := proto.Unmarshal([]byte(msg.Payload), &rm); err != nil {
				logger.Errorw("could not unmarshal RTC message on rtcchan", err)
				stats.PromMessageCounter.WithLabelValues("rtc", "failure").Add(1)
				continue
			}
			if err := r.handleRTCMessage(&rm); err != nil {
				logger.Errorw("error processing RTC message", err)
				stats.PromMessageCounter.WithLabelValues("rtc", "failure").Add(1)
				continue
			}
			stats.PromMessageCounter.WithLabelValues("rtc", "success").Add(1)
		}
	}
}

func (r *RedisRouter) handleSignalMessage(sm *livekit.SignalNodeMessage) error {
	connectionId := sm.ConnectionId

	r.lock.RLock()
	resSink := r.responseChannels[connectionId]
	r.lock.RUnlock()

	// if a client closed the channel, then sent more messages after that,
	if resSink == nil {
		return nil
	}

	switch rmb := sm.Message.(type) {
	case *livekit.SignalNodeMessage_Response:
		// logger.Debugw("forwarding signal message",
		//	"connID", connectionId,
		//	"type", fmt.Sprintf("%T", rmb.Response.Message))
		if err := resSink.WriteMessage(rmb.Response); err != nil {
			return err
		}

	case *livekit.SignalNodeMessage_EndSession:
		// logger.Debugw("received EndSession, closing signal connection",
		//	"connID", connectionId)
		resSink.Close()
	}
	return nil
}

func (r *RedisRouter) handleRTCMessage(rm *livekit.RTCNodeMessage) error {
	pKey := rm.ParticipantKey

	switch rmb := rm.Message.(type) {
	case *livekit.RTCNodeMessage_StartSession:
		// RTC session should start on this node
		if err := r.startParticipantRTC(rmb.StartSession, pKey); err != nil {
			return errors.Wrap(err, "could not start participant")
		}

	case *livekit.RTCNodeMessage_Request:
		r.lock.RLock()
		requestChan := r.requestChannels[pKey]
		r.lock.RUnlock()
		if requestChan == nil {
			return ErrChannelClosed
		}
		if err := requestChan.WriteMessage(rmb.Request); err != nil {
			return err
		}

	default:
		// route it to handler
		if r.onRTCMessage != nil {
			roomName, identity, err := parseParticipantKey(pKey)
			if err != nil {
				return err
			}
			r.onRTCMessage(r.ctx, roomName, identity, rm)
		}
	}
	return nil
}
