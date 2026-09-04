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
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

func TestLocalNodeKeepalive(t *testing.T) {
	n, err := routing.NewLocalNode(nil)
	require.NoError(t, err)

	// a node that has just started has missed nothing yet, so it must not read
	// as unready before its first ping has had a chance to arrive
	require.Less(t, n.SecondsSinceKeepalive(), 1.0)

	time.Sleep(600 * time.Millisecond)
	require.Greater(t, n.SecondsSinceKeepalive(), 0.5)

	n.UpdateKeepalive()
	require.Less(t, n.SecondsSinceKeepalive(), 0.5)
}

// The liveness probe reads this, so a node that has never sampled has to report
// staleness rather than panic on a nil Stats.
func TestLocalNodeWithoutStats(t *testing.T) {
	n, err := routing.NewLocalNodeFromNodeProto(&livekit.Node{Id: "node-test"})
	require.NoError(t, err)

	require.True(t, math.IsInf(n.SecondsSinceNodeStatsUpdate(), 1))
}

// What the liveness probe reads is the age of the node's own sample, so
// sampling has to be what refreshes it.
func TestLocalNodeStatsClock(t *testing.T) {
	require.NoError(t, prometheus.Init("test", livekit.NodeType_SERVER))

	n, err := routing.NewLocalNode(nil)
	require.NoError(t, err)

	n.SetStats(&livekit.NodeStats{UpdatedAt: time.Now().Add(-time.Minute).Unix()})
	require.Greater(t, n.SecondsSinceNodeStatsUpdate(), 30.0)

	require.True(t, n.UpdateNodeStats())
	// stats carry a unix second, so a fresh sample still reads up to a second old
	require.Less(t, n.SecondsSinceNodeStatsUpdate(), 2.0)
}
