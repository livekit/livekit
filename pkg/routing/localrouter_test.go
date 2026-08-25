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
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
)

// There is nowhere for a room to have moved to on a single node, so clearing
// one always did clear it. Saying so is what lets the room's own records be
// deleted at all on a deployment that routes locally.
func TestLocalRouterClearsRoomState(t *testing.T) {
	r := routing.NewLocalRouter(testNode(t), nil, nil, config.DefaultNodeStatsConfig)

	cleared, err := r.ClearRoomState(context.Background(), "test-room")
	require.NoError(t, err)
	require.True(t, cleared)
}
