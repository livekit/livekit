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

package buffer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
)

func TestDataStats(t *testing.T) {
	stats := NewDataStats(DataStatsParam{WindowDuration: time.Second})

	time.Sleep(time.Millisecond)
	r := stats.ToProtoAggregateOnly()
	require.Equal(t, r.StartTime.AsTime().UnixNano(), stats.startTime.UnixNano())
	require.NotZero(t, r.EndTime)
	require.NotZero(t, r.Duration)
	r.StartTime = nil
	r.EndTime = nil
	r.Duration = 0
	require.True(t, proto.Equal(r, &livekit.RTPStats{}))

	stats.Update(100, time.Now().UnixNano())
	r = stats.ToProtoActive()
	require.EqualValues(t, 100, r.Bytes)
	require.NotZero(t, r.Bitrate)

	// wait for window duration
	time.Sleep(time.Second)
	r = stats.ToProtoActive()
	require.True(t, proto.Equal(r, &livekit.RTPStats{}))
	stats.Stop()
	r = stats.ToProtoAggregateOnly()
	require.EqualValues(t, 100, r.Bytes)
	require.NotZero(t, r.Bitrate)
}

func TestDataStatsActiveBitrateAndDuration(t *testing.T) {
	stats := NewDataStats(DataStatsParam{WindowDuration: 10 * time.Second})

	// 1000 bytes over a 1.5 second window is 8000 bits / 1.5 s
	stats.windowStart = time.Now().Add(-1500 * time.Millisecond).UnixNano()
	stats.windowBytes = 1000

	r := stats.ToProtoActive()
	require.EqualValues(t, 1000, r.Bytes)
	require.InDelta(t, 1.5, r.Duration, 0.1)
	require.InDelta(t, 8000/1.5, r.Bitrate, 0.1*8000/1.5)
}
