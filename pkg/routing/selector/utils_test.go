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
