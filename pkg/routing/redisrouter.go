package routing

import (
	"bytes"
	"context"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

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
	LocalRouter

	rc        redis.UniversalClient
	ctx       context.Context
	isStarted atomic.Bool
	nodeMu    sync.RWMutex
	// previous stats for computing averages
	prevStats *livekit.NodeStats

	pubsub *redis.PubSub
	cancel func()
}

func NewRedisRouter(currentNode LocalNode, rc redis.UniversalClient) *RedisRouter {
	rr := &RedisRouter{
		LocalRouter: *NewLocalRouter(currentNode),
		rc:          rc,
	}
	rr.ctx, rr.cancel = context.WithCancel(context.Background())
	return rr
}

func (r *RedisRouter) RegisterNode() error {
	r.nodeMu.RLock()
	data, err := proto.Marshal((*livekit.Node)(r.currentNode))
	r.nodeMu.RUnlock()
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

	// create a new connection id
	connectionID = livekit.ConnectionID(utils.NewGuid("CO_"))
	pKey := participantKey(roomName, pi.Identity)

	// map signal & rtc nodes
	if err = r.setParticipantSignalNode(connectionID, r.currentNode.Id); err != nil {
		return
	}

	sink := NewRTCNodeSink(r.rc, livekit.NodeID(rtcNode.Id), pKey)

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

	// index by connectionID, since there may be multiple connections for the participant
	resChan := r.getOrCreateMessageChannel(r.responseChannels, string(connectionID))
	return connectionID, sink, resChan, nil
}

func (r *RedisRouter) WriteParticipantRTC(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) error {
	pkey := participantKey(roomName, identity)
	rtcNode, err := r.getParticipantRTCNode(pkey)
	if err != nil {
		return err
	}

	rtcSink := NewRTCNodeSink(r.rc, livekit.NodeID(rtcNode), pkey)
	msg.ParticipantKey = string(participantKey(roomName, identity))
	return r.writeRTCMessage(rtcSink, msg)
}

func (r *RedisRouter) WriteRoomRTC(ctx context.Context, roomName livekit.RoomName, msg *livekit.RTCNodeMessage) error {
	node, err := r.GetNodeForRoom(ctx, roomName)
	if err != nil {
		return err
	}
	msg.ParticipantKey = string(participantKey(roomName, ""))
	return r.WriteNodeRTC(ctx, node.Id, msg)
}

func (r *RedisRouter) WriteNodeRTC(_ context.Context, rtcNodeID string, msg *livekit.RTCNodeMessage) error {
	rtcSink := NewRTCNodeSink(r.rc, livekit.NodeID(rtcNodeID), livekit.ParticipantKey(msg.ParticipantKey))
	return r.writeRTCMessage(rtcSink, msg)
}

func (r *RedisRouter) startParticipantRTC(ss *livekit.StartSession, participantKey livekit.ParticipantKey) error {
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

	if err := r.setParticipantRTCNode(participantKey, rtcNode.Id); err != nil {
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
	requestChan, ok := r.requestChannels[string(participantKey)]
	r.lock.RUnlock()
	if ok {
		requestChan.Close()
	}

	pi, err := ParticipantInitFromStartSession(ss, r.currentNode.Region)
	if err != nil {
		return err
	}

	reqChan := r.getOrCreateMessageChannel(r.requestChannels, string(participantKey))
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

	// wait until worker is running
	select {
	case <-workerStarted:
		return nil
	case <-time.After(3 * time.Second):
		return errors.New("Unable to start redis router")
	}
}

func (r *RedisRouter) Drain() {
	r.nodeMu.Lock()
	r.currentNode.State = livekit.NodeState_SHUTTING_DOWN
	r.nodeMu.Unlock()
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

func (r *RedisRouter) setParticipantRTCNode(participantKey livekit.ParticipantKey, nodeID string) error {
	err := r.rc.Set(r.ctx, participantRTCKey(participantKey), nodeID, participantMappingTTL).Err()
	if err != nil {
		err = errors.Wrap(err, "could not set rtc node")
	}
	return err
}

func (r *RedisRouter) setParticipantSignalNode(connectionID livekit.ConnectionID, nodeID string) error {
	if err := r.rc.Set(r.ctx, participantSignalKey(connectionID), nodeID, participantMappingTTL).Err(); err != nil {
		return errors.Wrap(err, "could not set signal node")
	}
	return nil
}

func (r *RedisRouter) getParticipantRTCNode(participantKey livekit.ParticipantKey) (string, error) {
	val, err := r.rc.Get(r.ctx, participantRTCKey(participantKey)).Result()
	if err == redis.Nil {
		err = ErrNodeNotFound
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

// update node stats and cleanup
func (r *RedisRouter) statsWorker() {
	goroutineDumped := false
	for r.ctx.Err() == nil {
		// update periodically
		select {
		case <-time.After(statsUpdateInterval):
			_ = r.WriteNodeRTC(context.Background(), r.currentNode.Id, &livekit.RTCNodeMessage{
				Message: &livekit.RTCNodeMessage_KeepAlive{},
			})
			r.nodeMu.RLock()
			stats := r.currentNode.Stats
			r.nodeMu.RUnlock()

			delaySeconds := time.Now().Unix() - stats.UpdatedAt
			if delaySeconds > statsMaxDelaySeconds {
				if !goroutineDumped {
					goroutineDumped = true
					buf := bytes.NewBuffer(nil)
					_ = pprof.Lookup("goroutine").WriteTo(buf, 2)
					logger.Errorw("status update delayed, possible deadlock", nil,
						"delay", delaySeconds,
						"goroutines", buf.String())
				}
			} else {
				goroutineDumped = false
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

	switch rmb := rm.Message.(type) {
	case *livekit.RTCNodeMessage_StartSession:
		// RTC session should start on this node
		if err := r.startParticipantRTC(rmb.StartSession, pKey); err != nil {
			return errors.Wrap(err, "could not start participant")
		}

	case *livekit.RTCNodeMessage_Request:
		r.lock.RLock()
		requestChan := r.requestChannels[string(pKey)]
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

		r.nodeMu.Lock()
		if r.prevStats == nil {
			r.prevStats = r.currentNode.Stats
		}
		updated, computedAvg, err := prometheus.GetUpdatedNodeStats(r.currentNode.Stats, r.prevStats)
		if err != nil {
			logger.Errorw("could not update node stats", err)
			r.nodeMu.Unlock()
			return err
		}
		r.currentNode.Stats = updated
		if computedAvg {
			r.prevStats = updated
		}
		r.nodeMu.Unlock()

		// TODO: check stats against config.Limit values
		if err := r.RegisterNode(); err != nil {
			logger.Errorw("could not update node", err)
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
