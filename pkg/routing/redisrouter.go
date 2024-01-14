// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package routing

import (
	"bytes"
	"context"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const (
	// expire participant mappings after a day
	participantMappingTTL = 24 * time.Hour
	statsUpdateInterval   = 2 * time.Second
	statsMaxDelaySeconds  = 30
)

var _ Router = (*RedisRouter)(nil)

// RedisRouter uses Redis pub/sub to route signaling messages across different nodes
// It relies on the RTC node to be the primary driver of the participant connection.
// Because
type RedisRouter struct {
	*LocalRouter

	rc        redis.UniversalClient
	ctx       context.Context
	isStarted atomic.Bool
	nodeMu    sync.RWMutex
	// previous stats for computing averages
	prevStats *livekit.NodeStats

	pubsub *redis.PubSub
	cancel func()
}

func NewRedisRouter(lr *LocalRouter, rc redis.UniversalClient) *RedisRouter {
	rr := &RedisRouter{
		LocalRouter: lr,
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
func (r *RedisRouter) StartParticipantSignal(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit) (res StartParticipantSignalResults, err error) {
	// find the node where the room is hosted at
	rtcNode, err := r.GetNodeForRoom(ctx, roomName)
	if err != nil {
		return
	}

	return r.StartParticipantSignalWithNodeID(ctx, roomName, pi, livekit.NodeID(rtcNode.Id))
}

func (r *RedisRouter) WriteNodeRTC(_ context.Context, rtcNodeID string, msg *livekit.RTCNodeMessage) error {
	rtcSink := NewRTCNodeSink(r.rc, livekit.NodeID(rtcNodeID), "ephemeral", livekit.ParticipantKey(msg.ParticipantKey), livekit.ParticipantKey(msg.ParticipantKeyB62))
	defer rtcSink.Close()
	return r.writeRTCMessage(rtcSink, msg)
}

func (r *LocalRouter) writeRTCMessage(sink MessageSink, msg *livekit.RTCNodeMessage) error {
	msg.SenderTime = time.Now().Unix()
	return sink.WriteMessage(msg)
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

	rtcChannel := rtcNodeChannel(livekit.NodeID(r.currentNode.Id))
	r.pubsub = r.rc.Subscribe(r.ctx, rtcChannel)

	close(startedChan)
	for msg := range r.pubsub.Channel() {
		if msg == nil {
			return
		}

		if msg.Channel == rtcChannel {
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

func (r *RedisRouter) handleRTCMessage(rm *livekit.RTCNodeMessage) error {
	switch rm.Message.(type) {
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
	}
	return nil
}
