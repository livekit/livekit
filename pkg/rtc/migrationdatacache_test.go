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
	state := cache.Add(pkt1)
	require.Equal(t, MigrationDataCacheStateWaiting, state)
	require.Empty(t, cache.Get())

	pkt2 := &livekit.DataPacket{Sequence: 11}
	state = cache.Add(pkt2)
	require.Equal(t, MigrationDataCacheStateDone, state)
	require.Empty(t, cache.Get())

	pkt3 := &livekit.DataPacket{Sequence: 12}
	state = cache.Add(pkt3)
	require.Equal(t, MigrationDataCacheStateDone, state)
	require.Empty(t, cache.Get())

	cache2 := NewMigrationDataCache(20, time.Now().Add(10*time.Millisecond))
	pkt4 := &livekit.DataPacket{Sequence: 22}
	time.Sleep(20 * time.Millisecond)
	state = cache2.Add(pkt4)
	require.Equal(t, MigrationDataCacheStateTimeout, state)
	require.Len(t, cache2.Get(), 1)
	require.Equal(t, uint32(22), cache2.Get()[0].Sequence)
}
