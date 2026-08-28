// Copyright 2023 LiveKit, Inc.
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

package selector_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

func TestIsAvailable(t *testing.T) {
	t.Run("still available", func(t *testing.T) {
		n := &livekit.Node{
			Stats: &livekit.NodeStats{
				UpdatedAt: time.Now().Unix() - 3,
			},
		}
		require.True(t, selector.IsAvailable(n))
	})

	t.Run("expired", func(t *testing.T) {
		n := &livekit.Node{
			Stats: &livekit.NodeStats{
				UpdatedAt: time.Now().Unix() - 20,
			},
		}
		require.False(t, selector.IsAvailable(n))
	})
}

// A node on its way out keeps registering itself for as long as it drains, so
// its stats stay as fresh as a serving node's. Freshness alone cannot tell the
// two apart, and only one of them can be given a room.
func TestCanHostRoom(t *testing.T) {
	fresh := func(state livekit.NodeState) *livekit.Node {
		return &livekit.Node{
			State: state,
			Stats: &livekit.NodeStats{
				UpdatedAt: time.Now().Unix(),
			},
		}
	}

	t.Run("serving", func(t *testing.T) {
		require.True(t, selector.CanHostRoom(fresh(livekit.NodeState_SERVING)))
	})

	t.Run("draining", func(t *testing.T) {
		require.False(t, selector.CanHostRoom(fresh(livekit.NodeState_SHUTTING_DOWN)))
	})

	t.Run("not yet serving", func(t *testing.T) {
		require.False(t, selector.CanHostRoom(fresh(livekit.NodeState_STARTING_UP)))
	})

	t.Run("stale", func(t *testing.T) {
		n := fresh(livekit.NodeState_SERVING)
		n.Stats.UpdatedAt = time.Now().Unix() - 20
		require.False(t, selector.CanHostRoom(n))
	})
}
