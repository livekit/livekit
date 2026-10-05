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

package routing

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/selector"
)

func TestSweepDue(t *testing.T) {
	now := time.Now()
	timeout := selector.DeadNodeTimeout
	// started a moment ago, and the worker counts that as not having heard
	// itself, which is what holds the first sweep off
	started := now.Add(-time.Second)
	long := now.Add(-2 * timeout)

	t.Run("not while this node is new", func(t *testing.T) {
		require.False(t, sweepDue(now, started, time.Time{}))
	})

	t.Run("not while the outage is recent", func(t *testing.T) {
		// the bus came back a moment ago, and the peers that were on it are
		// still registering as stale as this node was
		require.False(t, sweepDue(now, now.Add(-timeout+time.Second), long))
	})

	t.Run("a whole timeout after the outage", func(t *testing.T) {
		require.True(t, sweepDue(now, now.Add(-timeout), long))
	})

	t.Run("not twice in a timeout", func(t *testing.T) {
		require.False(t, sweepDue(now, long, now.Add(-timeout+time.Second)))
		require.True(t, sweepDue(now, long, now.Add(-timeout)))
	})

	t.Run("first sweep is not held up by a zero last sweep", func(t *testing.T) {
		require.True(t, sweepDue(now, long, time.Time{}))
	})
}

// a node that cannot hear its own keepalive is in no position to decide that
// anybody else is dead
func TestRemoveDeadNodesNeedsThisNodeToBeHeard(t *testing.T) {
	node, err := NewLocalNode(nil)
	require.NoError(t, err)
	node.SetStats(&livekit.NodeStats{
		UpdatedAt: time.Now().Add(-time.Hour).Unix(),
	})

	// nothing is listening there, so a router that gets as far as reading the
	// registry fails here rather than reaping against whatever redis is up
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer rc.Close()

	r := NewRedisRouter(
		NewLocalRouter(node, nil, nil, config.DefaultNodeStatsConfig),
		rc,
		nil,
	)

	require.NoError(t, r.RemoveDeadNodes(), "a node this far behind on its own stats should not have read the registry at all")
}
