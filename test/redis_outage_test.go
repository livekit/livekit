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

package test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/testutils"
)

// These cover what a sustained redis outage does to a multi-node cluster, from
// https://github.com/livekit/livekit/issues/4663: liveness that does not follow
// redis, a drain that finishes, and a cluster that still has capacity when the
// outage ends.

// The endpoints that split apart what `GET /` used to answer on its own.
// /healthz answers for the process, /readyz for whether the node should be
// given work. `GET /` keeps what it did before, for compatibility.
const (
	healthzPath = "/healthz"
	readyzPath  = "/readyz"
)

// a probe that hangs is a probe that failed, same as a kubelet's timeoutSeconds
var probeClient = &http.Client{Timeout: 2 * time.Second}

// probe fetches one of the node's health endpoints. A node that cannot be
// reached at all reports 0, which every caller here treats like any other
// non-200.
func probe(s *service.LivekitServer, path string) int {
	res, err := probeClient.Get(fmt.Sprintf("http://localhost:%d%s", s.HTTPPort(), path))
	if err != nil {
		return 0
	}
	defer res.Body.Close()
	return res.StatusCode
}

func nodesInRedis() ([]*livekit.Node, error) {
	rc := redisClient()
	defer rc.Close()

	vals, err := rc.HGetAll(context.Background(), routing.NodesKey).Result()
	if err != nil {
		return nil, err
	}

	nodes := make([]*livekit.Node, 0, len(vals))
	for _, v := range vals {
		n := &livekit.Node{}
		if err := proto.Unmarshal([]byte(v), n); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// Liveness and readiness are one endpoint today, and it reports node stats
// staleness -- which on a redis-routed node is only refreshed by a keepalive
// ping that round-trips through redis. An outage therefore fails liveness on
// every node at once. Only readiness should follow redis.
func TestMultiNodeRedisOutageKeepsNodesLive(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	rd := startControlledRedis(t)

	s1, s2, finish := setupMultiNodeTest("TestMultiNodeRedisOutageKeepsNodesLive")
	defer finish()
	servers := []*service.LivekitServer{s1, s2}

	for _, s := range servers {
		require.Equal(t, http.StatusOK, probe(s, healthzPath), "node %s", s.Node().Id)
		require.Equal(t, http.StatusOK, probe(s, readyzPath), "node %s", s.Node().Id)
	}

	rd.stop()

	// well past the 4s staleness threshold that takes the shared endpoint down
	// today, and past a kubelet probe budget of three failures ten seconds apart
	const outageWatch = 10 * time.Second
	unready := map[string]bool{}
	for deadline := time.Now().Add(outageWatch); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		for _, s := range servers {
			require.Equal(t, http.StatusOK, probe(s, healthzPath),
				"node %s failed liveness during a redis outage, so kubernetes would kill it", s.Node().Id)
			if probe(s, readyzPath) != http.StatusOK {
				unready[s.Node().Id] = true
			}
		}
	}
	require.Len(t, unready, len(servers),
		"nodes stayed ready with redis unreachable, so they keep being handed new participants")

	// nothing was ever wrong with these processes: with redis back, and without
	// touching the servers, they are ready again
	rd.start(t)
	testutils.WithTimeout(t, func() string {
		for _, s := range servers {
			if code := probe(s, readyzPath); code != http.StatusOK {
				return fmt.Sprintf("node %s still returns %d after redis came back", s.Node().Id, code)
			}
		}
		return ""
	})
}

// A node told to shut down during the outage has to finish anyway: the drain
// needs a deadline, and once redis is back the node has to leave the registry.
// Stop(force=false) waits for participants who leave over signalling this node
// routes through redis, so without a deadline it waits for something that
// cannot happen while the keepalive worker keeps re-registering the node as
// SHUTTING_DOWN -- a state nothing sets back.
func TestMultiNodeRedisOutageDrainsAndDeregisters(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	rd := startControlledRedis(t)

	// compressed from what an operator would set, the same way the probe period
	// below is. it is the absolute deadline that ends this drain: redis is back
	// before the node has waited long enough to give up on being unreachable,
	// and a node that gave up mid-outage could not have reached redis to
	// deregister either
	const drainTimeout = 10 * time.Second

	s1, s2, finish := setupMultiNodeTestWithConfig("TestMultiNodeRedisOutageDrainsAndDeregisters",
		func(c *config.Config) { c.Shutdown.DrainTimeout = drainTimeout })
	defer finish()

	c1 := createRTCClientWithToken(joinToken("outage-drain1", "outage-a", nil), defaultServerPort, testRTCServicePathv0, nil)
	c2 := createRTCClientWithToken(joinToken("outage-drain2", "outage-b", nil), secondServerPort, testRTCServicePathv0, nil)
	defer stopClients(c1, c2)
	waitUntilConnected(t, c1, c2)

	// a room lives on whichever node the selector picked for it, so shut down one
	// that has participants to drain and leave the other to carry the cluster
	victim, survivor := s1, s2
	if !victim.RoomManager().HasParticipants() {
		victim, survivor = s2, s1
	}
	require.True(t, victim.RoomManager().HasParticipants(), "no node is hosting participants")

	// the participants cannot cooperate with the drain: the signal path they
	// would leave over is itself routed through redis
	rd.stop()
	drained := make(chan struct{})
	stopAt := time.Now()
	var drainTook time.Duration
	go func() {
		victim.Stop(false)
		drainTook = time.Since(stopAt)
		close(drained)
	}()
	// finish() forces the drain if it is still running when the test ends
	t.Cleanup(func() { <-drained })

	// the outage outlasts Drain()'s attempt to mark the node as draining, then
	// redis comes back with an empty registry
	time.Sleep(2 * time.Second)
	rd.start(t)

	// the node says it is going away for as long as it drains, rather than only
	// once the drain ends -- a node that still advertises SERVING while it waits
	// keeps being handed the rooms it is trying to get rid of
	testutils.WithTimeout(t, func() string {
		nodes, err := nodesInRedis()
		if err != nil {
			return err.Error()
		}
		for _, n := range nodes {
			if n.Id == victim.Node().Id {
				if n.State != livekit.NodeState_SHUTTING_DOWN {
					return fmt.Sprintf("the draining node advertises %s", n.State)
				}
				return ""
			}
		}
		return "the draining node has not re-registered yet"
	}, 6*time.Second)

	select {
	case <-drained:
		// a shutdown that returned before the deadline never drained at all,
		// which passes the registry checks below just as well as draining does
		require.GreaterOrEqual(t, drainTook, drainTimeout,
			"node %s stopped without waiting for the participants it was draining", victim.Node().Id)
	case <-time.After(testutils.ConnectTimeout):
		t.Fatalf("node %s is still draining", victim.Node().Id)
	}

	// the surviving node re-registers and keeps the cluster schedulable, and the
	// drained one took itself out rather than staying on as SHUTTING_DOWN
	testutils.WithTimeout(t, func() string {
		nodes, err := nodesInRedis()
		if err != nil {
			return err.Error()
		}
		var alive *livekit.Node
		for _, n := range nodes {
			if n.Id == victim.Node().Id {
				return fmt.Sprintf("the drained node is still registered, as %s", n.State)
			}
			if n.Id == survivor.Node().Id {
				alive = n
			}
		}
		if alive == nil {
			return "the surviving node is not registered"
		}
		if alive.State != livekit.NodeState_SERVING {
			return fmt.Sprintf("the surviving node is %s", alive.State)
		}
		return ""
	})
}

// The other deadline, which is the one the outage itself trips: with no
// absolute timeout configured, a node that cannot hear its own keepalive still
// has to stop, because the participants it is waiting for have no signalling
// path to leave over. Redis stays down for the whole drain here, so nothing but
// that clock can end it.
func TestMultiNodeRedisOutageEndsAnUnreachableDrain(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	rd := startControlledRedis(t)

	// compressed the same way the deadline in the test above is, and below the
	// poll interval, so the first poll of the drain is the one that gives up
	const unreachableTimeout = 3 * time.Second

	s1, s2, finish := setupMultiNodeTestWithConfig("TestMultiNodeRedisOutageEndsAnUnreachableDrain",
		func(c *config.Config) {
			c.Shutdown.DrainTimeout = 0
			c.Shutdown.UnreachableDrainTimeout = unreachableTimeout
			// a participant whose signal relay gives up is one that leaves on
			// its own, which would end the drain without the deadline having
			// anything to do with it. held well past the end of the test, so
			// that the only way out of the drain is the deadline
			c.SignalRelay.RetryTimeout = 5 * time.Minute
		})
	defer finish()

	c1 := createRTCClientWithToken(joinToken("outage-unreachable1", "outage-a", nil), defaultServerPort, testRTCServicePathv0, nil)
	c2 := createRTCClientWithToken(joinToken("outage-unreachable2", "outage-b", nil), secondServerPort, testRTCServicePathv0, nil)
	defer stopClients(c1, c2)
	waitUntilConnected(t, c1, c2)

	victim := s1
	if !victim.RoomManager().HasParticipants() {
		victim = s2
	}
	require.True(t, victim.RoomManager().HasParticipants(), "no node is hosting participants")

	rd.stop()
	drained := make(chan struct{})
	stopAt := time.Now()
	go func() {
		victim.Stop(false)
		close(drained)
	}()
	// finish() forces the drain if it is still running when the test ends
	t.Cleanup(func() { <-drained })

	select {
	case <-drained:
		t.Logf("the drain gave up after %s of an unreachable node", time.Since(stopAt).Round(time.Millisecond))
	// most of this budget is the shutdown that follows the drain -- closing
	// rooms against a redis that is still down costs seconds per key -- so it
	// is generous on purpose. a drain with no working deadline never ends at
	// all, so it fails here whatever the machine is doing
	case <-time.After(unreachableTimeout + 2*testutils.ConnectTimeout):
		t.Fatalf("node %s is still draining a node that cannot reach redis", victim.Node().Id)
	}
}

// failsLivenessProbe replays the shape of the chart's default liveness probe --
// failureThreshold consecutive failures a period apart and the container is
// restarted -- at a compressed period, so the test does not sit out a real
// kubelet's 30s budget.
func failsLivenessProbe(s *service.LivekitServer, path string, until time.Time) bool {
	const (
		period           = time.Second
		failureThreshold = 3
	)
	failures := 0
	for time.Now().Before(until) {
		time.Sleep(period)
		if probe(s, path) == http.StatusOK {
			failures = 0
			continue
		}
		failures++
		if failures == failureThreshold {
			return true
		}
	}
	return false
}

// The whole cascade, driven end to end: redis goes away, every node fails the
// liveness probe at the same moment because the only health endpoint measures a
// keepalive that round-trips through redis, kubernetes restarts the fleet, each
// node enters a graceful drain it cannot finish, and when redis returns they
// re-register as SHUTTING_DOWN and never leave it. What comes back up is a
// cluster with no schedulable node.
//
// A redis outage is a loss of capacity for as long as it lasts. It should not
// outlive itself.
func TestMultiNodeRedisOutageDoesNotKillTheCluster(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	rd := startControlledRedis(t)

	s1, s2, finish := setupMultiNodeTest("TestMultiNodeRedisOutageDoesNotKillTheCluster")
	defer finish()
	servers := []*service.LivekitServer{s1, s2}

	c1 := createRTCClientWithToken(joinToken("outage-room1", "outage-a", nil), defaultServerPort, testRTCServicePathv0, nil)
	c2 := createRTCClientWithToken(joinToken("outage-room2", "outage-b", nil), secondServerPort, testRTCServicePathv0, nil)
	defer stopClients(c1, c2)
	waitUntilConnected(t, c1, c2)

	const (
		outage = 15 * time.Second
		// the kubelet keeps watching a little past the outage, long enough for a
		// node that was about to fail its last probe to do so
		watch = outage + 5*time.Second
	)
	until := time.Now().Add(watch)

	// stand in for the kubelet watching each pod: a failed liveness probe means
	// SIGTERM, which for livekit-server is a graceful shutdown
	var shutdowns sync.WaitGroup
	// every probe reports what it decided, so that reading them all back is proof
	// that none is still about to restart a node
	decided := make(chan string, len(servers))
	for _, s := range servers {
		shutdowns.Add(1)
		go func(s *service.LivekitServer) {
			defer shutdowns.Done()
			if !failsLivenessProbe(s, healthzPath, until) {
				decided <- ""
				return
			}
			decided <- s.Node().Id
			s.Stop(false)
		}(s)
	}
	// finish() forces any shutdown still running when the test ends
	t.Cleanup(shutdowns.Wait)

	rd.stop()
	time.Sleep(outage)
	rd.start(t)

	var killed []string
	for range servers {
		if id := <-decided; id != "" {
			killed = append(killed, id)
		}
	}
	t.Logf("%d of %d nodes failed liveness and were restarted during a %s outage",
		len(killed), len(servers), outage)

	// with redis back, the cluster still has capacity
	testutils.WithTimeout(t, func() string {
		nodes, err := nodesInRedis()
		if err != nil {
			return err.Error()
		}
		if len(selector.GetAvailableNodes(nodes)) > 0 {
			return ""
		}
		states := make([]string, 0, len(nodes))
		for _, n := range nodes {
			states = append(states, fmt.Sprintf("%s=%s", n.Id, n.State))
		}
		return fmt.Sprintf("no schedulable node left, registry holds %v", states)
	}, 15*time.Second)

	// and takes rooms again
	_, err := roomClient.CreateRoom(contextWithToken(createRoomToken()),
		&livekit.CreateRoomRequest{Name: "outage-room3"})
	require.NoError(t, err)

	require.Empty(t, killed, "the outage cost healthy processes their liveness")
}
