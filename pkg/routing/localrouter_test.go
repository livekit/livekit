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
