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

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/testutils"
)

// These cover what a sustained redis outage does to a multi-node cluster. They
// describe the behavior we want rather than the behavior we have, so as of
// https://github.com/livekit/livekit/issues/4663 all three of them fail.

// The endpoints the fix is expected to add, splitting apart what `GET /` does on
// its own today. /healthz answers for the process, /readyz for whether the node
// should be given work. `GET /` keeps what it does now, for compatibility.
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
// Today Stop(force=false) waits for participants that cannot leave, with no
// deadline and no way to configure one, while the keepalive worker keeps
// re-registering the node as SHUTTING_DOWN -- a state nothing sets back.
func TestMultiNodeRedisOutageDrainsAndDeregisters(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	rd := startControlledRedis(t)

	s1, s2, finish := setupMultiNodeTest("TestMultiNodeRedisOutageDrainsAndDeregisters")
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
	go func() {
		victim.Stop(false)
		close(drained)
	}()
	// finish() forces the drain if it is still running when the test ends
	t.Cleanup(func() { <-drained })

	// the outage outlasts Drain()'s attempt to mark the node as draining, then
	// redis comes back with an empty registry
	time.Sleep(2 * time.Second)
	rd.start(t)

	select {
	case <-drained:
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

// livenessPath is what a kubelet has to aim a liveness probe at: /healthz once
// the fix adds it, and until then `GET /`, the only health surface there is.
func livenessPath(s *service.LivekitServer) string {
	if probe(s, healthzPath) != http.StatusNotFound {
		return healthzPath
	}
	return "/"
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
		path := livenessPath(s)
		shutdowns.Add(1)
		go func(s *service.LivekitServer) {
			defer shutdowns.Done()
			if !failsLivenessProbe(s, path, until) {
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
