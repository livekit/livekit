package buffer

import (
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
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
