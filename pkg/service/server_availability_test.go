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
	"github.com/livekit/livekit-server/pkg/routing"
)

// newAvailabilityTestServer builds a server around a node whose state and
// stats the test controls directly.
func newAvailabilityTestServer(t *testing.T, node *livekit.Node, limits config.LimitConfig) *LivekitServer {
	t.Helper()

	localNode, err := routing.NewLocalNodeFromNodeProto(node)
	require.NoError(t, err)

	return &LivekitServer{
		config:      &config.Config{Limit: limits},
		currentNode: localNode,
	}
}

func currentStatsNow() *livekit.NodeStats {
	return &livekit.NodeStats{
		UpdatedAt: time.Now().Unix(),
	}
}

func requestAvailability(s *LivekitServer) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.defaultHandler(rec, httptest.NewRequest(http.MethodGet, "/availability", nil))
	return rec
}

func TestAvailabilityServingAndFresh(t *testing.T) {
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: currentStatsNow(),
	}, config.LimitConfig{})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "Available", rec.Body.String())
}

func TestAvailabilityShuttingDown(t *testing.T) {
	// the case the issue asks for: a draining node accepts its running
	// sessions but the load balancer must stop sending new joins here
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SHUTTING_DOWN,
		Stats: currentStatsNow(),
	}, config.LimitConfig{})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "shutting down")
}

func TestAvailabilityStartingUp(t *testing.T) {
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_STARTING_UP,
		Stats: currentStatsNow(),
	}, config.LimitConfig{})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "starting up")
}

func TestAvailabilitySuspended(t *testing.T) {
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SUSPENDED,
		Stats: currentStatsNow(),
	}, config.LimitConfig{})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "suspended")
}

func TestAvailabilityStaleStats(t *testing.T) {
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt: time.Now().Add(-10 * time.Second).Unix(),
		},
	}, config.LimitConfig{})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "stats are stale")
}

func TestAvailabilityLimitsReached(t *testing.T) {
	// node is serving with fresh stats, but has hit its configured track
	// limit, so it takes no new joins
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:    time.Now().Unix(),
			NumTracksIn:  400,
			NumTracksOut: 100,
		},
	}, config.LimitConfig{NumTracks: 500})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "configured limit")
}

func TestAvailabilityLimitsNotReached(t *testing.T) {
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:    time.Now().Unix(),
			NumTracksIn:  10,
			NumTracksOut: 10,
		},
	}, config.LimitConfig{NumTracks: 500})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAvailabilityNoStatsYet(t *testing.T) {
	// right after startup there are no stats; a serving node is available, as
	// node selection treats it
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SERVING,
	}, config.LimitConfig{})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAvailabilityShuttingDownWithoutStats(t *testing.T) {
	// a node that starts draining before its first stats update must still
	// report unavailable, so the load balancer stops sending joins
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SHUTTING_DOWN,
	}, config.LimitConfig{})

	rec := requestAvailability(s)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "shutting down")
}

func TestHealthCheckStillWorks(t *testing.T) {
	// the root endpoint keeps its original meaning: stats freshness only
	s := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SHUTTING_DOWN,
		Stats: currentStatsNow(),
	}, config.LimitConfig{})

	rec := httptest.NewRecorder()
	s.defaultHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// a stale node fails both
	rec = httptest.NewRecorder()
	s2 := newAvailabilityTestServer(t, &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{UpdatedAt: time.Now().Add(-10 * time.Second).Unix()},
	}, config.LimitConfig{})
	s2.defaultHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusNotAcceptable, rec.Code)
}
