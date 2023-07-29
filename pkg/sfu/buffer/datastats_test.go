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
