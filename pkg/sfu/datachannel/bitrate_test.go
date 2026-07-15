package datachannel

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var busyModes = []BitrateMode{BitrateModeBusyOnly, BitrateModeExcludeIdleDrain}

func modeName(m BitrateMode) string {
	switch m {
	case BitrateModeBusyOnly:
		return "busyOnly"
	case BitrateModeExcludeIdleDrain:
		return "excludeIdleDrain"
	case BitrateModeWallClock:
		return "wallClock"
	default:
		return "unknown"
	}
}

// 1 KB written every second, each fully drained before the next write: never
// backlogged, so there is no estimate.
func TestBitrateCalculatorSparseFastAck(t *testing.T) {
	for _, mode := range busyModes {
		t.Run(modeName(mode), func(t *testing.T) {
			c := NewBitrateCalculator(BitrateDuration, BitrateWindow, mode)
			require.NotNil(t, c)

			t0 := time.Now()
			for i := 0; i < 5; i++ {
				ts := t0.Add(time.Duration(i) * time.Second)
				c.AddBytes(1000, 1000, ts)
				_, ok := c.Bitrate(ts)
				require.False(t, ok)
			}
		})
	}
}

// Standing backlog of 1000 bytes; every 100ms write 800 and 800 drain, sampled
// buffer 1000+800=1800. 800 B / 100ms = 64000 bps.
func TestBitrateCalculatorSustainedBacklog(t *testing.T) {
	for _, mode := range busyModes {
		t.Run(modeName(mode), func(t *testing.T) {
			c := NewBitrateCalculator(BitrateDuration, BitrateWindow, mode)

			t0 := time.Now()
			c.AddBytes(800, 1800, t0)
			for i := 1; i <= 30; i++ {
				c.AddBytes(800, 1800, t0.Add(time.Duration(i)*100*time.Millisecond))
			}

			bitrate, ok := c.Bitrate(t0.Add(3 * time.Second))
			require.True(t, ok)
			require.InDelta(t, 64000, bitrate, 2000)
		})
	}
}

// Buffer only climbs and nothing drains (stalled connection): the estimate
// converges to ~0.
func TestBitrateCalculatorStalledConnection(t *testing.T) {
	for _, mode := range busyModes {
		t.Run(modeName(mode), func(t *testing.T) {
			c := NewBitrateCalculator(BitrateDuration, BitrateWindow, mode)

			t0 := time.Now()
			buffered := 0
			for i := 0; i < 20; i++ {
				buffered += 1000 // wrote 1000, nothing drained
				c.AddBytes(1000, buffered, t0.Add(time.Duration(i)*50*time.Millisecond))
			}

			bitrate, ok := c.Bitrate(t0.Add(time.Second))
			require.True(t, ok)
			require.Less(t, bitrate, 1000)
		})
	}
}

// A single write that drains to empty is not backlogged at the next sample, so
// no estimate is produced.
func TestBitrateCalculatorFlushNotCounted(t *testing.T) {
	for _, mode := range busyModes {
		t.Run(modeName(mode), func(t *testing.T) {
			c := NewBitrateCalculator(BitrateDuration, BitrateWindow, mode)

			t0 := time.Now()
			c.AddBytes(100, 100, t0)
			ts := t0.Add(100 * time.Millisecond)
			c.AddBytes(0, 0, ts)
			_, ok := c.Bitrate(ts)
			require.False(t, ok)
		})
	}
}

// An idle drain followed by a backlog: BusyOnly counts the idle-drained bytes
// over busy time only and reports a higher rate than ExcludeIdleDrain.
func TestBitrateCalculatorMixedIdleThenBacklog(t *testing.T) {
	off := NewBitrateCalculator(BitrateDuration, BitrateWindow, BitrateModeBusyOnly)
	on := NewBitrateCalculator(BitrateDuration, BitrateWindow, BitrateModeExcludeIdleDrain)

	t0 := time.Now()
	feed := func(written, buffered int, ts time.Time) {
		off.AddBytes(written, buffered, ts)
		on.AddBytes(written, buffered, ts)
	}

	// 5000 drains during an idle gap (not backlogged), then a sustained backlog
	// of 800 every 100ms over a standing 5000.
	feed(5000, 5000, t0)
	feed(5000, 5000, t0.Add(100*time.Millisecond))
	for i := 1; i <= 5; i++ {
		feed(800, 5800, t0.Add(time.Duration(100+i*100)*time.Millisecond))
	}
	queryTs := t0.Add(700 * time.Millisecond)

	rOff, okOff := off.Bitrate(queryTs)
	rOn, okOn := on.Bitrate(queryTs)
	require.True(t, okOff)
	require.True(t, okOn)
	require.Greater(t, rOff, rOn)
}

// No observable buffer: throughput over elapsed time. 1000 B every 100ms =
// 80000 bps.
func TestBitrateCalculatorWallClock(t *testing.T) {
	c := NewBitrateCalculator(BitrateDuration, BitrateWindow, BitrateModeWallClock)

	t0 := time.Now()
	for i := 0; i < 20; i++ {
		c.AddBytes(1000, 0, t0.Add(time.Duration(i)*100*time.Millisecond))
	}

	bitrate, ok := c.Bitrate(t0.Add(2 * time.Second))
	require.True(t, ok)
	require.InDelta(t, 80000, bitrate, 8000)
}
