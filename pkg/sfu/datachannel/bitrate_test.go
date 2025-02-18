package datachannel

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBitrateCalculator(t *testing.T) {
	c := NewBitrateCalculator(BitrateDuration, BitrateWindow)
	require.NotNil(t, c)

	t0 := time.Now()
	c.AddBytes(100, 0, t0)
	// bytes buffered
	c.AddBytes(100, 100, t0.Add(50*time.Millisecond))
	bitrate, ok := c.Bitrate(t0.Add(50 * time.Millisecond))
	require.Equal(t, 0, bitrate)
	require.False(t, ok)
	// 50 bytes sent (50 bytes buffer flushed)
	c.AddBytes(100, 50, t0.Add(time.Second))

	// 250 bytes sent in 1 second
	bitrate, ok = c.Bitrate(t0.Add(time.Second))
	require.Equal(t, 2000, bitrate)
	require.True(t, ok)

	// silence for long time
	t1 := t0.Add(2 * BitrateDuration)
	// 150 bytes sent (50 bytes buffer flushed)
	c.AddBytes(100, 0, t1)
	bitrate, ok = c.Bitrate(t1.Add(time.Second))
	require.Equal(t, 1200, bitrate)
	require.True(t, ok)
}
