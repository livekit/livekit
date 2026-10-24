package rtc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestMigrationDataCache_Add(t *testing.T) {
	expiredAt := time.Now().Add(100 * time.Millisecond)
	cache := NewMigrationDataCache(10, expiredAt)

	pkt1 := &livekit.DataPacket{Sequence: 9}
	state := cache.Add(pkt1, 0)
	require.Equal(t, MigrationDataCacheStateWaiting, state)
	require.Empty(t, cache.Get())

	pkt2 := &livekit.DataPacket{Sequence: 11}
	state = cache.Add(pkt2, 0)
	require.Equal(t, MigrationDataCacheStateDone, state)
	require.Empty(t, cache.Get())

	pkt3 := &livekit.DataPacket{Sequence: 12}
	state = cache.Add(pkt3, 0)
	require.Equal(t, MigrationDataCacheStateDone, state)
	require.Empty(t, cache.Get())

	cache2 := NewMigrationDataCache(20, time.Now().Add(10*time.Millisecond))
	pkt4 := &livekit.DataPacket{Sequence: 22}
	time.Sleep(20 * time.Millisecond)
	state = cache2.Add(pkt4, 0)
	require.Equal(t, MigrationDataCacheStateTimeout, state)
	require.Len(t, cache2.Get(), 1)
	require.Equal(t, uint32(22), cache2.Get()[0].Sequence)
}

func TestMigrationDataCache_MaxSize(t *testing.T) {
	// the cache should not grow past the size budget even if the expiry is far in the future
	cache := NewMigrationDataCache(10, time.Now().Add(time.Minute))

	pktSize := 1000
	seq := uint32(12)
	state := MigrationDataCacheStateWaiting
	for ; state == MigrationDataCacheStateWaiting; seq++ {
		state = cache.Add(&livekit.DataPacket{Sequence: seq}, pktSize)
	}

	require.Equal(t, MigrationDataCacheStateTimeout, state)
	require.LessOrEqual(t, cache.Size(), migrationDataCacheMaxSize+pktSize)
	require.Len(t, cache.Get(), migrationDataCacheMaxSize/pktSize+1)

	// once full, further packets are dropped, including the continuous one
	require.Equal(t, MigrationDataCacheStateTimeout, cache.Add(&livekit.DataPacket{Sequence: 11}, pktSize))
}
