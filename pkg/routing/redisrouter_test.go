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
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/rpc/rpcfakes"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
)

type pingSubscription struct {
	ch chan *rpc.KeepalivePing
}

func (s *pingSubscription) Channel() <-chan *rpc.KeepalivePing { return s.ch }

func (s *pingSubscription) Close() error {
	close(s.ch)
	return nil
}

// startRedisRouter runs a redis router whose keepalive pings are delivered by
// hand, over a redis client that cannot connect to anything. Nothing here needs
// a redis, which is the point: the node has to keep reporting on itself when
// there is no redis to be had.
func startRedisRouter(
	t *testing.T,
	node routing.LocalNode,
	kps *rpcfakes.FakeKeepalivePubSub,
	statsUpdateInterval time.Duration,
) (*routing.RedisRouter, chan *rpc.KeepalivePing) {
	pings := &pingSubscription{ch: make(chan *rpc.KeepalivePing, 1)}
	kps.SubscribePingReturns(pings, nil)

	nsc := config.DefaultNodeStatsConfig
	nsc.StatsUpdateInterval = statsUpdateInterval

	rc := deadRedis()
	r := routing.NewRedisRouter(routing.NewLocalRouter(node, nil, nil, nsc), rc, kps)
	require.NoError(t, r.Start())
	t.Cleanup(func() {
		r.Stop()
		_ = pings.Close()
		_ = rc.Close()
	})

	return r, pings.ch
}

// deadRedis points at nothing listening, and does not wait around to find out.
func deadRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		MaxRetries:  -1,
		DialTimeout: 10 * time.Millisecond,
	})
}

func testNode() *routingfakes.FakeLocalNode {
	node := &routingfakes.FakeLocalNode{}
	node.CloneReturns(&livekit.Node{Id: "node-test"})
	node.UpdateNodeStatsReturns(true)
	return node
}

// Node stats are what the liveness check reads, and on a redis-routed node they
// used to be refreshed only by a keepalive ping round-tripping through redis
// pub/sub, so a redis outage stalled them on every node at once.
func TestRedisRouterStatsDoNotDependOnKeepalive(t *testing.T) {
	node := testNode()
	_, _ = startRedisRouter(t, node, &rpcfakes.FakeKeepalivePubSub{}, testStatsInterval)

	require.Eventually(t, func() bool {
		return node.UpdateNodeStatsCallCount() >= 5
	}, time.Second, 10*time.Millisecond, "node stats stalled with no keepalive to refresh them")

	require.Zero(t, node.UpdateKeepaliveCallCount(), "no ping was delivered")
}

// The sample has to be taken before the ping goes out, because the ping is what
// triggers the registration that publishes it, and a sample taken afterwards
// can lose the race and put last interval's stats in redis.
func TestRedisRouterSamplesStatsBeforePinging(t *testing.T) {
	var lock sync.Mutex
	var calls []string
	record := func(what string) {
		lock.Lock()
		defer lock.Unlock()
		calls = append(calls, what)
	}

	node := testNode()
	node.UpdateNodeStatsStub = func() bool {
		record("stats")
		return true
	}
	kps := &rpcfakes.FakeKeepalivePubSub{}
	kps.PublishPingCalls(func(context.Context, livekit.NodeID, *rpc.KeepalivePing) error {
		record("ping")
		return nil
	})
	_, _ = startRedisRouter(t, node, kps, testStatsInterval)

	require.Eventually(t, func() bool {
		lock.Lock()
		defer lock.Unlock()
		return len(calls) >= 2
	}, time.Second, 10*time.Millisecond)

	lock.Lock()
	defer lock.Unlock()
	require.Equal(t, []string{"stats", "ping"}, calls[:2])
}

// The round trip is still worth measuring, being the node's proof that it can
// receive the messages routed to it. It just answers for readiness now, and no
// longer samples stats of its own.
func TestRedisRouterKeepaliveFollowsPings(t *testing.T) {
	node := testNode()
	// an interval that cannot tick during the test, so that a stats sample can
	// only come from the ping. the staleness gate a ping passes is measured
	// against this interval, so a longer one only loosens it
	_, pings := startRedisRouter(t, node, &rpcfakes.FakeKeepalivePubSub{}, time.Minute)

	pings <- &rpc.KeepalivePing{Timestamp: time.Now().Unix()}
	require.Eventually(t, func() bool {
		return node.UpdateKeepaliveCallCount() == 1
	}, time.Second, 10*time.Millisecond, "keepalive did not follow the ping")

	require.Zero(t, node.UpdateNodeStatsCallCount(), "the ping sampled stats of its own")
}

// Stop is final here too, the same way it is for the LocalRouter this embeds:
// the context a stopped router cancelled is not replaced.
func TestRedisRouterStopIsFinal(t *testing.T) {
	r, _ := startRedisRouter(t, testNode(), &rpcfakes.FakeKeepalivePubSub{}, time.Minute)

	r.Stop()
	require.ErrorIs(t, r.Start(), routing.ErrRouterStopped)
}

// A router that never started still has a context to let go of, and still has
// to refuse to start afterwards.
func TestRedisRouterStopBeforeStart(t *testing.T) {
	rc := deadRedis()
	defer rc.Close()

	r := routing.NewRedisRouter(
		routing.NewLocalRouter(testNode(), nil, nil, config.DefaultNodeStatsConfig),
		rc,
		&rpcfakes.FakeKeepalivePubSub{},
	)

	r.Stop()
	require.ErrorIs(t, r.Start(), routing.ErrRouterStopped)
}
