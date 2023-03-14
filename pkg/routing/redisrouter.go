package routing

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const (
	// expire participant mappings after a day
	participantMappingTTL = 24 * time.Hour
	statsUpdateInterval   = 2 * time.Second
	statsMaxDelaySeconds  = 30
)

// RedisRouter uses Redis pub/sub to route signaling messages across different nodes
// It relies on the RTC node to be the primary driver of the participant connection.
// Because
type RedisRouter struct {
	*LocalRouter

	rc             redis.UniversalClient
	usePSRPCSignal bool
	ctx            context.Context
	isStarted      atomic.Bool
	// previous stats for computing averages
	prevStats *livekit.NodeStats

	pubsub *redis.PubSub
	cancel func()
}

func NewRedisRouter(config *config.Config, lr *LocalRouter, rc redis.UniversalClient) *RedisRouter {
	rr := &RedisRouter{
		LocalRouter:    lr,
		rc:             rc,
		usePSRPCSignal: config.SignalRelay.Enabled,
	}
	rr.ctx, rr.cancel = context.WithCancel(context.Background())
	return rr
}

func (r *RedisRouter) RegisterNode() error {
	r.lock.RLock()
	data, err := proto.Marshal((*livekit.Node)(r.currentNode))
	r.lock.RUnlock()
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

func (r *RedisRouter) GetNodeForRoom(_ context.Context, roomName livekit.RoomName) (*livekit.Node, error) {
	nodeID, err := r.rc.HGet(r.ctx, NodeRoomKey, string(roomName)).Result()
	if err == redis.Nil {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, errors.Wrap(err, "could not get node for room")
	}

	return r.GetNode(livekit.NodeID(nodeID))
}

func (r *RedisRouter) SetNodeForRoom(_ context.Context, roomName livekit.RoomName, nodeID livekit.NodeID) error {
	return r.rc.HSet(r.ctx, NodeRoomKey, string(roomName), string(nodeID)).Err()
}

func (r *RedisRouter) ClearRoomState(_ context.Context, roomName livekit.RoomName) error {
	if err := r.rc.HDel(context.Background(), NodeRoomKey, string(roomName)).Err(); err != nil {
		return errors.Wrap(err, "could not clear room state")
	}
	return nil
}

func (r *RedisRouter) GetNode(nodeID livekit.NodeID) (*livekit.Node, error) {
	data, err := r.rc.HGet(r.ctx, NodesKey, string(nodeID)).Result()
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
func (r *RedisRouter) StartParticipantSignal(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit) (connectionID livekit.ConnectionID, reqSink MessageSink, resSource MessageSource, err error) {
	// find the node where the room is hosted at
	rtcNode, err := r.GetNodeForRoom(ctx, roomName)
	if err != nil {
		return
	}

	if r.usePSRPCSignal {
		return r.StartParticipantSignalWithNodeID(ctx, roomName, pi, livekit.NodeID(rtcNode.Id))
	}

	// create a new connection id
	connectionID = livekit.ConnectionID(utils.NewGuid("CO_"))
	pKey := participantKeyLegacy(roomName, pi.Identity)
	pKeyB62 := participantKey(roomName, pi.Identity)

	// map signal & rtc nodes
	if err = r.setParticipantSignalNode(connectionID, r.currentNode.Id); err != nil {
		return
	}

	// index by connectionID, since there may be multiple connections for the participant
	// set up response channel before sending StartSession and be ready to receive responses.
	resChan := r.getOrCreateMessageChannel(r.responseChannels, string(connectionID))

	sink := NewRTCNodeSink(r.rc, livekit.NodeID(rtcNode.Id), pKey, pKeyB62)

	// serialize claims
	ss, err := pi.ToStartSession(roomName, connectionID)
	if err != nil {
		return
	}

	// sends a message to start session
	err = sink.WriteMessage(ss)
	if err != nil {
		return
	}

	return connectionID, sink, resChan, nil
}

func (r *RedisRouter) WriteParticipantRTC(ctx context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) error {
	pkey := participantKeyLegacy(roomName, identity)
	pkeyB62 := participantKey(roomName, identity)
	rtcNode, err := r.getParticipantRTCNode(pkey, pkeyB62)
	if err != nil {
		return err
	}

	return r.writeNodeRTC(ctx, rtcNode, roomName, identity, msg)
}

func (r *RedisRouter) WriteRoomRTC(ctx context.Context, roomName livekit.RoomName, msg *livekit.RTCNodeMessage) error {
	node, err := r.GetNodeForRoom(ctx, roomName)
	if err != nil {
		return err
	}

	return r.writeNodeRTC(ctx, node.Id, roomName, "", msg)
}

func (r *RedisRouter) writeNodeRTC(
	ctx context.Context,
	rtcNodeID string,
	roomName livekit.RoomName,
	identity livekit.ParticipantIdentity,
	msg *livekit.RTCNodeMessage,
) error {
	pkey := participantKeyLegacy(roomName, identity)
	pkeyB62 := participantKey(roomName, identity)

	msg.ParticipantKey = string(pkey)
	msg.ParticipantKeyB62 = string(pkeyB62)
	msg.SenderTime = time.Now().Unix()
	msg.RoomName = string(roomName)
	msg.Identity = string(identity)

	rtcSink := NewRTCNodeSink(r.rc, livekit.NodeID(rtcNodeID), pkey, pkeyB62)
	defer rtcSink.Close()
	return rtcSink.WriteMessage(msg)
}

func (r *RedisRouter) startParticipantRTC(ss *livekit.StartSession, participantKey livekit.ParticipantKey, participantKeyB62 livekit.ParticipantKey) error {
	prometheus.IncrementParticipantRtcInit(1)
	// find the node where the room is hosted at
	rtcNode, err := r.GetNodeForRoom(r.ctx, livekit.RoomName(ss.RoomName))
	if err != nil {
		return err
	}

	if rtcNode.Id != r.currentNode.Id {
		err = ErrIncorrectRTCNode
		logger.Errorw("called participant on incorrect node", err,
			"rtcNode", rtcNode,
		)
		return err
	}

	if err := r.setParticipantRTCNode(participantKey, participantKeyB62, rtcNode.Id); err != nil {
		return err
	}

	// find signal node to send responses back
	signalNode, err := r.getParticipantSignalNode(livekit.ConnectionID(ss.ConnectionId))
	if err != nil {
		return err
	}

	// treat it as a new participant connecting
	if r.onNewParticipant == nil {
		return ErrHandlerNotDefined
	}

	// we do not want to re-use the same response sink
	// the previous rtc worker thread is still consuming off of it.
	// we'll want to sever the connection and switch to the new one
	r.lock.RLock()
	var requestChan *MessageChannel
	var ok bool
	var pkey livekit.ParticipantKey
	if participantKeyB62 != "" {
		requestChan, ok = r.requestChannels[string(participantKeyB62)]
		pkey = participantKeyB62
	} else {
		requestChan, ok = r.requestChannels[string(participantKey)]
		pkey = participantKey
	}
	r.lock.RUnlock()
	if ok {
		requestChan.Close()
	}

	pi, err := ParticipantInitFromStartSession(ss, r.currentNode.Region)
	if err != nil {
		return err
	}

	reqChan := r.getOrCreateMessageChannel(r.requestChannels, string(pkey))
	resSink := NewSignalNodeSink(r.rc, livekit.NodeID(signalNode), livekit.ConnectionID(ss.ConnectionId))
	go func() {
		err := r.onNewParticipant(
			r.ctx,
			livekit.RoomName(ss.RoomName),
			*pi,
			reqChan,
			resSink,
		)
		if err != nil {
			logger.Errorw("could not handle new participant", err,
				"room", ss.RoomName,
				"participant", ss.Identity,
			)
			// cleanup request channels
			reqChan.Close()
			resSink.Close()
		}
	}()
	return nil
}

func (r *RedisRouter) Start() error {
	if r.isStarted.Swap(true) {
		return nil
	}

	workerStarted := make(chan struct{})
	go r.statsWorker()
	go r.redisWorker(workerStarted)

	if err := r.LocalRouter.Start(); err != nil {
		return err
	}

	// wait until worker is running
	select {
	case <-workerStarted:
		return nil
	case <-time.After(3 * time.Second):
		return errors.New("Unable to start redis router")
	}
}

func (r *RedisRouter) Drain() {
	r.lock.Lock()
	r.currentNode.State = livekit.NodeState_SHUTTING_DOWN
	r.lock.Unlock()
	if err := r.RegisterNode(); err != nil {
		logger.Errorw("failed to mark as draining", err, "nodeID", r.currentNode.Id)
	}
}

func (r *RedisRouter) Stop() {
	if !r.isStarted.Swap(false) {
		return
	}
	logger.Debugw("stopping RedisRouter")
	_ = r.pubsub.Close()
	_ = r.UnregisterNode()
	r.cancel()
}

func (r *RedisRouter) statsWorker() {
	statsUpdateTicker := time.NewTicker(statsUpdateInterval)
	defer statsUpdateTicker.Stop()

	for {
		select {
		case <-statsUpdateTicker.C:
			if err := r.updateNodeStats(); err != nil {
				logger.Errorw("could not update node", err)
			}
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *RedisRouter) updateNodeStats() error {
	r.lock.Lock()
	if r.prevStats == nil {
		r.prevStats = r.currentNode.Stats
	}
	updated, computedAvg, err := prometheus.GetUpdatedNodeStats(r.currentNode.Stats, r.prevStats)
	if err != nil {
		logger.Errorw("could not update node stats", err)
		r.lock.Unlock()
		return err
	}
	r.currentNode.Stats = updated
	if computedAvg {
		r.prevStats = updated
	}
	r.lock.Unlock()

	// TODO: check stats against config.Limit values

	return r.RegisterNode()
}

func (r *RedisRouter) setParticipantRTCNode(participantKey livekit.ParticipantKey, participantKeyB62 livekit.ParticipantKey, nodeID string) (err error) {
	if participantKey != "" {
		err = r.rc.Set(r.ctx, participantRTCKey(participantKey), nodeID, participantMappingTTL).Err()
	}
	if participantKeyB62 != "" {
		err = r.rc.Set(r.ctx, participantRTCKey(participantKeyB62), nodeID, participantMappingTTL).Err()
	}
	if err != nil {
		return errors.Wrap(err, "could not set rtc node")
	}
	return nil
}

func (r *RedisRouter) setParticipantSignalNode(connectionID livekit.ConnectionID, nodeID string) error {
	if err := r.rc.Set(r.ctx, participantSignalKey(connectionID), nodeID, participantMappingTTL).Err(); err != nil {
		return errors.Wrap(err, "could not set signal node")
	}
	return nil
}

func (r *RedisRouter) getParticipantRTCNode(participantKey livekit.ParticipantKey, participantKeyB62 livekit.ParticipantKey) (string, error) {
	var val string
	var err error
	if participantKeyB62 != "" {
		val, err = r.rc.Get(r.ctx, participantRTCKey(participantKeyB62)).Result()
		if err == redis.Nil {
			val, err = r.rc.Get(r.ctx, participantRTCKey(participantKey)).Result()
			if err == redis.Nil {
				err = ErrNodeNotFound
			}
		}
	} else {
		val, err = r.rc.Get(r.ctx, participantRTCKey(participantKey)).Result()
		if err == redis.Nil {
			err = ErrNodeNotFound
		}
	}
	return val, err
}

func (r *RedisRouter) getParticipantSignalNode(connectionID livekit.ConnectionID) (nodeID string, err error) {
	val, err := r.rc.Get(r.ctx, participantSignalKey(connectionID)).Result()
	if err == redis.Nil {
		err = ErrNodeNotFound
	}
	return val, err
}

// worker that consumes redis messages intended for this node
func (r *RedisRouter) redisWorker(startedChan chan struct{}) {
	defer func() {
		logger.Debugw("finishing redisWorker", "nodeID", r.currentNode.Id)
	}()
	logger.Debugw("starting redisWorker", "nodeID", r.currentNode.Id)

	sigChannel := signalNodeChannel(livekit.NodeID(r.currentNode.Id))
	rtcChannel := rtcNodeChannel(livekit.NodeID(r.currentNode.Id))
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
				prometheus.MessageCounter.WithLabelValues("signal", "failure").Add(1)
				continue
			}
			if err := r.handleSignalMessage(&sm); err != nil {
				logger.Errorw("error processing signal message", err)
				prometheus.MessageCounter.WithLabelValues("signal", "failure").Add(1)
				continue
			}
			prometheus.MessageCounter.WithLabelValues("signal", "success").Add(1)
		} else if msg.Channel == rtcChannel {
			rm := livekit.RTCNodeMessage{}
			if err := proto.Unmarshal([]byte(msg.Payload), &rm); err != nil {
				logger.Errorw("could not unmarshal RTC message on rtcchan", err)
				prometheus.MessageCounter.WithLabelValues("rtc", "failure").Add(1)
				continue
			}
			if err := r.handleRTCMessage(&rm); err != nil {
				logger.Errorw("error processing RTC message", err)
				prometheus.MessageCounter.WithLabelValues("rtc", "failure").Add(1)
				continue
			}
			prometheus.MessageCounter.WithLabelValues("rtc", "success").Add(1)
		}
	}
}

func (r *RedisRouter) handleSignalMessage(sm *livekit.SignalNodeMessage) error {
	connectionID := sm.ConnectionId

	r.lock.RLock()
	resSink := r.responseChannels[connectionID]
	r.lock.RUnlock()

	// if a client closed the channel, then sent more messages after that,
	if resSink == nil {
		return nil
	}

	switch rmb := sm.Message.(type) {
	case *livekit.SignalNodeMessage_Response:
		// logger.Debugw("forwarding signal message",
		//	"connID", connectionID,
		//	"type", fmt.Sprintf("%T", rmb.Response.Message))
		if err := resSink.WriteMessage(rmb.Response); err != nil {
			return err
		}

	case *livekit.SignalNodeMessage_EndSession:
		// logger.Debugw("received EndSession, closing signal connection",
		//	"connID", connectionID)
		resSink.Close()
	}
	return nil
}

func (r *RedisRouter) handleRTCMessage(rm *livekit.RTCNodeMessage) error {
	pKey := livekit.ParticipantKey(rm.ParticipantKey)
	pKeyB62 := livekit.ParticipantKey(rm.ParticipantKeyB62)

	switch rmb := rm.Message.(type) {
	case *livekit.RTCNodeMessage_StartSession:
		// RTC session should start on this node
		if err := r.startParticipantRTC(rmb.StartSession, pKey, pKeyB62); err != nil {
			return errors.Wrap(err, "could not start participant")
		}

	case *livekit.RTCNodeMessage_Request:
		r.lock.RLock()
		var requestChan *MessageChannel
		if pKeyB62 != "" {
			requestChan = r.requestChannels[string(pKeyB62)]
		} else {
			requestChan = r.requestChannels[string(pKey)]
		}
		r.lock.RUnlock()
		if requestChan == nil {
			return ErrChannelClosed
		}
		if err := requestChan.WriteMessage(rmb.Request); err != nil {
			return err
		}

	case *livekit.RTCNodeMessage_KeepAlive:
		if time.Since(time.Unix(rm.SenderTime, 0)) > statsUpdateInterval {
			logger.Infow("keep alive too old, skipping", "senderTime", rm.SenderTime)
			break
		}

		if err := r.updateNodeStats(); err != nil {
			logger.Errorw("could not update node", err)
		}

	default:
		// route it to handler
		if r.onRTCMessage != nil {
			var roomName livekit.RoomName
			var identity livekit.ParticipantIdentity
			var err error
			if pKeyB62 != "" {
				roomName, identity, err = parseParticipantKey(pKeyB62)
			}
			if err != nil || pKeyB62 == "" {
				roomName, identity, err = parseParticipantKeyLegacy(pKey)
			}
			if err != nil {
				return err
			}
			r.onRTCMessage(r.ctx, roomName, identity, rm)
		}
	}
	return nil
}
