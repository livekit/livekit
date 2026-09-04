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
	"time"

	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

const (
	// hash of node_id => Node proto
	NodesKey = "nodes"

	// hash of room_name => node_id
	NodeRoomKey = "room_node_map"
)

var _ Router = (*RedisRouter)(nil)

// RedisRouter uses Redis pub/sub to route signaling messages across different nodes
// It relies on the RTC node to be the primary driver of the participant connection.
// Because
type RedisRouter struct {
	*LocalRouter

	rc        redis.UniversalClient
	kps       rpc.KeepalivePubSub
	ctx       context.Context
	isStarted atomic.Bool

	cancel func()
}

func NewRedisRouter(lr *LocalRouter, rc redis.UniversalClient, kps rpc.KeepalivePubSub) *RedisRouter {
	rr := &RedisRouter{
		LocalRouter: lr,
		rc:          rc,
		kps:         kps,
	}
	rr.ctx, rr.cancel = context.WithCancel(context.Background())
	return rr
}

func (r *RedisRouter) RegisterNode() error {
	data, err := proto.Marshal(r.currentNode.Clone())
	if err != nil {
		return err
	}
	if err := r.rc.HSet(r.ctx, NodesKey, string(r.currentNode.NodeID()), data).Err(); err != nil {
		return errors.Wrap(err, "could not register node")
	}
	return nil
}

func (r *RedisRouter) UnregisterNode() error {
	// could be called after Stop(), so we'd want to use an unrelated context
	return r.rc.HDel(context.Background(), NodesKey, string(r.currentNode.NodeID())).Err()
}

func (r *RedisRouter) RemoveDeadNodes() error {
	// a node that cannot hear its own keepalive is looking at a bus that has
	// stopped carrying registrations, not at a fleet that has died
	if !r.hearsItself() {
		return nil
	}

	nodes, err := r.ListNodes()
	if err != nil {
		return err
	}

	dead := make([]string, 0, len(nodes))
	for _, n := range nodes {
		// this node's own entry is the one it is least able to judge: its
		// registration goes stale while its local stats do not
		if n.Id != string(r.currentNode.NodeID()) && selector.IsDead(n) {
			dead = append(dead, n.Id)
		}
	}
	if len(dead) == 0 {
		return nil
	}

	return r.rc.HDel(r.ctx, NodesKey, dead...).Err()
}

// hearsItself reports whether this node's own keepalive has come back to it
// recently enough for what it reads about the others to mean anything.
// StatsMaxDelay is what the stats worker already takes for this path being
// wedged; half the timeout is the ceiling over that, so that a peer always has
// to be at least the other half further behind than this node before it can be
// buried, whatever the delay is set to.
func (r *RedisRouter) hearsItself() bool {
	fresh := min(r.nodeStatsConfig.StatsMaxDelay, selector.DeadNodeTimeout/2)
	return r.currentNode.SecondsSinceNodeStatsUpdate() <= fresh.Seconds()
}

// GetNodeForRoom finds the node where the room is hosted at
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

func (r *RedisRouter) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (res *livekit.Room, err error) {
	rtcNode, err := r.GetNodeForRoom(ctx, livekit.RoomName(req.Name))
	if err != nil {
		return
	}

	return r.CreateRoomWithNodeID(ctx, req, livekit.NodeID(rtcNode.Id))
}

// StartParticipantSignal signal connection sets up paths to the RTC node, and starts to route messages to that message queue
func (r *RedisRouter) StartParticipantSignal(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit) (res StartParticipantSignalResults, err error) {
	rtcNode, err := r.GetNodeForRoom(ctx, roomName)
	if err != nil {
		return
	}

	return r.StartParticipantSignalWithNodeID(ctx, roomName, pi, livekit.NodeID(rtcNode.Id))
}

func (r *RedisRouter) Start() error {
	if r.isStarted.Swap(true) {
		return nil
	}

	workerStarted := make(chan error)
	go r.statsWorker()
	go r.deadNodeWorker()
	go r.keepaliveWorker(workerStarted)

	// wait until worker is running
	return <-workerStarted
}

func (r *RedisRouter) Drain() {
	r.currentNode.SetState(livekit.NodeState_SHUTTING_DOWN)
	if err := r.RegisterNode(); err != nil {
		logger.Errorw("failed to mark as draining", err, "nodeID", r.currentNode.NodeID())
	}
}

func (r *RedisRouter) Stop() {
	if !r.isStarted.Swap(false) {
		return
	}
	logger.Debugw("stopping RedisRouter")
	_ = r.UnregisterNode()
	r.cancel()
}

// update node stats and cleanup
func (r *RedisRouter) statsWorker() {
	goroutineDumped := false
	for r.ctx.Err() == nil {
		// update periodically
		select {
		case <-time.After(r.nodeStatsConfig.StatsUpdateInterval):
			r.kps.PublishPing(r.ctx, r.currentNode.NodeID(), &rpc.KeepalivePing{Timestamp: time.Now().Unix()})

			delaySeconds := r.currentNode.SecondsSinceNodeStatsUpdate()
			if delaySeconds > r.nodeStatsConfig.StatsMaxDelay.Seconds() {
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

// sweepDue reports whether a sweep may run, which takes a whole timeout since
// this node last could not hear itself, and another since the last sweep, that
// second one being what holds this to the pace of what it watches for rather
// than to the pace of the ticker.
func sweepDue(now, unheard, lastSweep time.Time) bool {
	return now.Sub(unheard) >= selector.DeadNodeTimeout &&
		now.Sub(lastSweep) >= selector.DeadNodeTimeout
}

// deadNodeWorker sweeps the registrations of nodes that are gone. A node killed
// outright never unregisters itself and nothing else deletes what it leaves
// behind, so this runs while the cluster does rather than only when a node
// happens to start.
func (r *RedisRouter) deadNodeWorker() {
	// sampled on the stats interval rather than on the timeout it guards: a
	// tick as coarse as that timeout cannot see an outage shorter than one,
	// which is the length that makes a peer look dead in the first place
	ticker := time.NewTicker(r.nodeStatsConfig.StatsUpdateInterval)
	defer ticker.Stop()

	// starting up counts as not having heard itself, so the first sweep comes a
	// whole timeout in -- as does the first after an outage, which gives the
	// peers coming back from it the time to re-register that this node took
	unheard := time.Now()
	var lastSweep time.Time
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			// the sweep asks this too, for whoever reaches it through the
			// Router interface; here it is asked to date the outage
			if !r.hearsItself() {
				unheard = now
				continue
			}
			if !sweepDue(now, unheard, lastSweep) {
				continue
			}

			lastSweep = now
			if err := r.RemoveDeadNodes(); err != nil {
				logger.Errorw("could not remove dead nodes", err)
			}
		}
	}
}

func (r *RedisRouter) keepaliveWorker(startedChan chan error) {
	pings, err := r.kps.SubscribePing(r.ctx, r.currentNode.NodeID())
	if err != nil {
		startedChan <- err
		return
	}
	close(startedChan)

	for ping := range pings.Channel() {
		if time.Since(time.Unix(ping.Timestamp, 0)) > r.nodeStatsConfig.StatsUpdateInterval {
			logger.Infow("keep alive too old, skipping", "timestamp", ping.Timestamp)
			continue
		}

		if !r.currentNode.UpdateNodeStats() {
			continue
		}

		// TODO: check stats against config.Limit values
		if err := r.RegisterNode(); err != nil {
			logger.Errorw("could not update node", err)
		}
	}
}
