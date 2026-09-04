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

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
)

// healthServer serves the probes off a node with its two clocks set by hand:
// how long since this process last ticked, and how long since its keepalive
// ping last came back to it.
func healthServer(t *testing.T) (*LivekitServer, *routingfakes.FakeLocalNode) {
	t.Helper()

	node := &routingfakes.FakeLocalNode{}
	node.StateReturns(livekit.NodeState_SERVING)

	return &LivekitServer{
		config:      &config.Config{NodeStats: config.DefaultNodeStatsConfig},
		currentNode: node,
	}, node
}

func statusOf(h http.HandlerFunc) int {
	return statusVia(h, "/")
}

func statusVia(h http.Handler, path string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code
}

// A redis outage stalls the keepalive on every node at once. If that failed
// liveness, kubernetes would restart the whole fleet over a dependency none of
// the processes had anything wrong with.
func TestLivenessIgnoresTheKeepalive(t *testing.T) {
	s, node := healthServer(t)
	node.SecondsSinceKeepaliveReturns(time.Minute.Seconds())

	require.Equal(t, http.StatusOK, statusOf(s.livenessCheck))
}

// Each probe has to be reachable at its own path, answering its own question.
func TestHealthRoutes(t *testing.T) {
	s, node := healthServer(t)
	node.SecondsSinceKeepaliveReturns((10 * time.Second).Seconds())

	mux := http.NewServeMux()
	s.setupHealthRoutes(mux)

	require.Equal(t, http.StatusOK, statusVia(mux, "/healthz"))
	require.Equal(t, http.StatusServiceUnavailable, statusVia(mux, "/readyz"))
}

// Liveness answers for the process alone, and a process that has stopped
// updating its own stats has stopped making progress.
func TestLivenessFailsWhenTheProcessStalls(t *testing.T) {
	s, node := healthServer(t)
	node.SecondsSinceNodeStatsUpdateReturns((config.DefaultNodeStatsConfig.StatsMaxDelay + time.Second).Seconds())

	require.Equal(t, http.StatusServiceUnavailable, statusOf(s.livenessCheck))
}

// Stats are only as fresh as the interval they are sampled on, so a node told
// to sample less often than the delay allows for must not fail liveness for it.
func TestLivenessAllowsALongerStatsInterval(t *testing.T) {
	s, node := healthServer(t)
	s.config.NodeStats.StatsUpdateInterval = time.Minute
	node.SecondsSinceNodeStatsUpdateReturns((45 * time.Second).Seconds())

	require.Equal(t, http.StatusOK, statusOf(s.livenessCheck))
}

// A draining node is still alive, so it keeps its liveness: restarting it would
// cut short the drain it was asked to perform.
func TestLivenessSurvivesDraining(t *testing.T) {
	s, node := healthServer(t)
	node.StateReturns(livekit.NodeState_SHUTTING_DOWN)

	require.Equal(t, http.StatusOK, statusOf(s.livenessCheck))
}

// Readiness is where the round trip through redis belongs: a node that cannot
// hear its own keepalive cannot route signalling either, and should be given no
// new work until it can.
func TestReadinessFollowsTheKeepalive(t *testing.T) {
	s, node := healthServer(t)
	require.Equal(t, http.StatusOK, statusOf(s.readinessCheck))

	node.SecondsSinceKeepaliveReturns((10 * time.Second).Seconds())
	require.Equal(t, http.StatusServiceUnavailable, statusOf(s.readinessCheck))
}

// The keepalive is only as frequent as the stats interval it rides on, so a
// node configured to ping less often must not be called unready for it.
func TestReadinessAllowsALongerStatsInterval(t *testing.T) {
	s, node := healthServer(t)
	s.config.NodeStats.StatsUpdateInterval = 10 * time.Second
	node.SecondsSinceKeepaliveReturns((11 * time.Second).Seconds())

	require.Equal(t, http.StatusOK, statusOf(s.readinessCheck))
}

// A shorter interval does not tighten the allowance below what the check has
// always given a node.
func TestReadinessKeepsItsFloor(t *testing.T) {
	s, node := healthServer(t)
	s.config.NodeStats.StatsUpdateInterval = 100 * time.Millisecond
	node.SecondsSinceKeepaliveReturns((3 * time.Second).Seconds())

	require.Equal(t, http.StatusOK, statusOf(s.readinessCheck))
}

func TestReadinessFailsWhileDraining(t *testing.T) {
	s, node := healthServer(t)
	node.StateReturns(livekit.NodeState_SHUTTING_DOWN)

	require.Equal(t, http.StatusServiceUnavailable, statusOf(s.readinessCheck))
}

// `GET /` keeps answering what it always has, so that deployments pointed at
// it, for liveness as much as for readiness, are not moved out from under.
func TestRootIsUnchanged(t *testing.T) {
	s, node := healthServer(t)
	require.Equal(t, http.StatusOK, statusOf(s.defaultHandler))

	node.StateReturns(livekit.NodeState_SHUTTING_DOWN)
	require.Equal(t, http.StatusOK, statusOf(s.defaultHandler),
		"a draining node still answers `GET /`, which predates /readyz")

	node.SecondsSinceKeepaliveReturns((10 * time.Second).Seconds())
	require.Equal(t, http.StatusNotAcceptable, statusOf(s.defaultHandler))
}
