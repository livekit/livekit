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

package prometheus

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestInitRoomIncludedStatsInvalidRegex(t *testing.T) {
	require.Error(t, InitRoomIncludedStats("test", livekit.NodeType_SERVER, "(["))
}

func TestInitRoomIncludedStatsEmptyIsNoop(t *testing.T) {
	require.NoError(t, InitRoomIncludedStats("test", livekit.NodeType_SERVER, ""))
}

func TestRoomIncludedTotal(t *testing.T) {
	require.NoError(t, Init("test", livekit.NodeType_SERVER))
	require.NoError(t, InitRoomIncludedStats("test", livekit.NodeType_SERVER, "^(loadtest|healthcheck)-"))
	t.Cleanup(func() { roomExcludeRegex.Store(nil) })

	totalBefore := gaugeValue(t, promRoomCurrent)
	includedBefore := gaugeValue(t, promRoomIncludedCurrent)

	for _, name := range []livekit.RoomName{"interview-1", "loadtest-42", "healthcheck-a"} {
		RoomStarted()
		RoomIncludedStarted(name)
	}
	// existing metric still counts every room
	require.Equal(t, totalBefore+3, gaugeValue(t, promRoomCurrent))
	require.Equal(t, includedBefore+1, gaugeValue(t, promRoomIncludedCurrent))

	RoomIncludedEnded("loadtest-42")
	require.Equal(t, includedBefore+1, gaugeValue(t, promRoomIncludedCurrent))

	RoomIncludedEnded("interview-1")
	require.Equal(t, includedBefore, gaugeValue(t, promRoomIncludedCurrent))

	for range 3 {
		RoomEnded(time.Time{})
	}
	RoomIncludedEnded("healthcheck-a")
	require.Equal(t, totalBefore, gaugeValue(t, promRoomCurrent))
	require.Equal(t, includedBefore, gaugeValue(t, promRoomIncludedCurrent))
}

func TestRoomIncludedDisabledIsSafe(t *testing.T) {
	roomExcludeRegex.Store(nil)
	// must not panic even if the gauge was never registered
	RoomIncludedStarted("interview-1")
	RoomIncludedEnded("interview-1")
}
