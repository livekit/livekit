// Copyright 2026 LiveKit, Inc.
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

package routing_test

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc/rpcfakes"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/testutils"
)

const testNodeID = livekit.NodeID("node-test")

func testNode(t *testing.T) routing.LocalNode {
	node, err := routing.NewLocalNodeFromNodeProto(&livekit.Node{Id: string(testNodeID)})
	require.NoError(t, err)
	return node
}

// liveRedis is a redis of this test's own, for the few things that cannot be
// shown without one. The keys a router writes to are fixed ones, and the rest
// of the suite flushes the redis it shares whenever a multi-node test ends, so
// a redis this test is alone on is the only one it can rely on.
func liveRedis(t *testing.T) *redis.Client {
	rc := redis.NewClient(testutils.StartRedis(t).Options())
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

func testRedisRouter(t *testing.T, rc redis.UniversalClient) *routing.RedisRouter {
	return routing.NewRedisRouter(
		routing.NewLocalRouter(testNode(t), nil, nil, config.DefaultNodeStatsConfig),
		rc,
		&rpcfakes.FakeKeepalivePubSub{},
	)
}

// A room can be moved off a draining node while that node still has it, and
// then the node finishing its drain must not take the mapping with it: the room
// it would clear is the one the live node is now hosting, and the next
// participant to look for it would be sent somewhere else again.
func TestRedisRouterClearsOnlyItsOwnRoomMapping(t *testing.T) {
	rc := liveRedis(t)
	ctx := context.Background()

	const room = livekit.RoomName("clear-room-state-test")

	r := testRedisRouter(t, rc)
	t.Cleanup(r.Stop)

	require.NoError(t, r.SetNodeForRoom(ctx, room, "node-other"))
	cleared, err := r.ClearRoomState(ctx, room)
	require.NoError(t, err)
	require.False(t, cleared, "a room routed to another node was reported as cleared")
	held, err := rc.HGet(ctx, routing.NodeRoomKey, string(room)).Result()
	require.NoError(t, err, "the room was cleared off the node that had taken it over")
	require.Equal(t, "node-other", held)

	require.NoError(t, r.SetNodeForRoom(ctx, room, testNodeID))
	cleared, err = r.ClearRoomState(ctx, room)
	require.NoError(t, err)
	require.True(t, cleared, "a room this node was on was not reported as cleared")
	_, err = rc.HGet(ctx, routing.NodeRoomKey, string(room)).Result()
	require.ErrorIs(t, err, redis.Nil, "the room was left routed to the node it was on")
}

// The rooms outlive the router on the way out: a node stops its router before
// the room manager closes what is left on it, and each of those rooms clears
// its own routing as it goes. A call made on the router's own context would
// find it cancelled and clear nothing.
func TestRedisRouterClearsRoomStateAfterStop(t *testing.T) {
	rc := liveRedis(t)
	ctx := context.Background()

	const room = livekit.RoomName("clear-after-stop-test")

	r := testRedisRouter(t, rc)

	require.NoError(t, r.SetNodeForRoom(ctx, room, testNodeID))
	r.Stop()

	cleared, err := r.ClearRoomState(ctx, room)
	require.NoError(t, err)
	require.True(t, cleared, "a stopped router left the room it was closing routed to itself")
}
