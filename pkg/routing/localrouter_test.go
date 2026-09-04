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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
)

const testStatsInterval = 20 * time.Millisecond

func startLocalRouter(t *testing.T, node *routingfakes.FakeLocalNode) *routing.LocalRouter {
	nsc := config.DefaultNodeStatsConfig
	nsc.StatsUpdateInterval = testStatsInterval

	r := routing.NewLocalRouter(node, nil, nil, nsc)
	require.NoError(t, r.Start())
	t.Cleanup(r.Stop)

	// only once it is sampling, so that a test which stops it can tell it did
	require.Eventually(t, func() bool {
		return node.UpdateNodeStatsCallCount() > 0
	}, time.Second, 10*time.Millisecond)

	return r
}

// A single-node deployment has no message bus to round-trip a ping through, so
// its router keeps the keepalive fresh itself. Otherwise every such node would
// report itself unready forever.
func TestLocalRouterKeepsKeepaliveFresh(t *testing.T) {
	node := &routingfakes.FakeLocalNode{}
	startLocalRouter(t, node)

	require.Eventually(t, func() bool {
		return node.UpdateKeepaliveCallCount() >= 3
	}, time.Second, 10*time.Millisecond)
}

// Stop has to stop the sampler. A worker left behind would go on sampling
// alongside the next one, which is enough to keep the stats history from
// covering a rate measurement window.
func TestLocalRouterStopsSampling(t *testing.T) {
	node := &routingfakes.FakeLocalNode{}
	r := startLocalRouter(t, node)

	r.Stop()
	sampled := settledSampleCount(t, node)

	time.Sleep(10 * testStatsInterval)
	require.Equal(t, sampled, node.UpdateNodeStatsCallCount())
}

// Stop is final, and says so: asking twice has to be as harmless as asking
// once, and starting again has to say it did not.
func TestLocalRouterStopIsFinal(t *testing.T) {
	node := &routingfakes.FakeLocalNode{}
	r := startLocalRouter(t, node)

	r.Stop()
	r.Stop()
	require.ErrorIs(t, r.Start(), routing.ErrRouterStopped)
	r.Stop()

	sampled := settledSampleCount(t, node)
	time.Sleep(10 * testStatsInterval)
	require.Equal(t, sampled, node.UpdateNodeStatsCallCount())
}

// settledSampleCount waits for sampling to stop, which a sample already under
// way when the router stopped can outlast by a tick.
func settledSampleCount(t *testing.T, node *routingfakes.FakeLocalNode) int {
	t.Helper()

	sampled := -1
	require.Eventually(t, func() bool {
		count := node.UpdateNodeStatsCallCount()
		settled := count == sampled
		sampled = count
		return settled
	}, time.Second, 5*testStatsInterval)

	return sampled
}
