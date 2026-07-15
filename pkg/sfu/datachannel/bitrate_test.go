package datachannel

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// busyModes are the two writer-facing modes; the pure scenarios below behave
// identically under both, so they are exercised with each.
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

// A fast channel fed sparsely (e.g. 1 KB every second, acknowledged in a few
// ms) is never backlogged, so there is nothing to measure the drain capacity
// against. The calculator must report "no estimate" rather than the low
// offered load, otherwise the latency-based drop threshold in the writer would
// collapse and drop bursts on a channel that is actually keeping up.
func TestBitrateCalculatorSparseFastAck(t *testing.T) {
	for _, mode := range busyModes {
		t.Run(modeName(mode), func(t *testing.T) {
			c := NewBitrateCalculator(BitrateDuration, BitrateWindow, mode)
			require.NotNil(t, c)

			t0 := time.Now()
			for i := 0; i < 5; i++ {
				ts := t0.Add(time.Duration(i) * time.Second)
				// The previous write was already SACKed, so sampled right after
				// this write the buffer holds only the 1000 bytes we just
				// enqueued: pre-write occupancy is zero => the channel was never
				// backlogged => no estimate.
				c.AddBytes(1000, 1000, ts)
				_, ok := c.Bitrate(ts)
				require.False(t, ok, "no backlog observed => no estimate")
			}
		})
	}
}

// Under a sustained backlog the buffer never drains to empty between writes, so
// the full inter-write interval is busy time. The reported rate then reflects
// the real drain capacity (bytes drained / busy time), independent of how much
// idle time surrounds the congested period.
func TestBitrateCalculatorSustainedBacklog(t *testing.T) {
	for _, mode := range busyModes {
		t.Run(modeName(mode), func(t *testing.T) {
			c := NewBitrateCalculator(BitrateDuration, BitrateWindow, mode)

			t0 := time.Now()
			// A standing backlog of 1000 bytes never fully drains. Every 100ms
			// we write 800 bytes and 800 drain, so the buffer sampled right
			// after each write is 1000 (standing) + 800 (just enqueued) = 1800;
			// pre-write occupancy is 1000 > 0 so the interval counts as busy.
			// 800 bytes / 100ms = 8000 B/s = 64000 bps.
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

// When the connection stalls (e.g. before ICE failure is detected) the app
// keeps writing but nothing is SACKed, so the buffer only climbs: pre-write
// occupancy stays > 0 (backlogged) while nothing drains, and the estimate
// converges to ~0 so the writer starts shedding load.
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

// A write that fully drains to empty before the next sample is not measurable
// capacity: we only know the drain finished sometime within the interval, not
// how long it took, so the interval is excluded (pre-write occupancy 0).
// Neither busy mode produces an estimate from it. Precise accounting of such
// intervals would need a drain-completion signal (e.g. OnBufferedAmountLow).
func TestBitrateCalculatorFlushNotCounted(t *testing.T) {
	for _, mode := range busyModes {
		t.Run(modeName(mode), func(t *testing.T) {
			c := NewBitrateCalculator(BitrateDuration, BitrateWindow, mode)

			t0 := time.Now()
			c.AddBytes(100, 100, t0) // 100 enqueued, nothing drained yet
			ts := t0.Add(100 * time.Millisecond)
			c.AddBytes(0, 0, ts) // the 100 flushed; pre-write occupancy 0 => excluded
			_, ok := c.Bitrate(ts)
			require.False(t, ok)
		})
	}
}

// In a mixed window -- bytes drained across an idle gap, then a sustained
// backlog -- busy-only counts the idle-drained bytes against the backlog's busy
// time and over-estimates the rate, while exclude-idle-drain does not. This is
// the gap the two modes exist to trade off.
func TestBitrateCalculatorMixedIdleThenBacklog(t *testing.T) {
	off := NewBitrateCalculator(BitrateDuration, BitrateWindow, BitrateModeBusyOnly)
	on := NewBitrateCalculator(BitrateDuration, BitrateWindow, BitrateModeExcludeIdleDrain)

	t0 := time.Now()
	feed := func(written, buffered int, ts time.Time) {
		off.AddBytes(written, buffered, ts)
		on.AddBytes(written, buffered, ts)
	}

	// Enqueue 5000, which fully drains during an idle gap, then enqueue another
	// 5000: this interval drained 5000 but was NOT backlogged (pre-write
	// occupancy 0).
	feed(5000, 5000, t0)
	feed(5000, 5000, t0.Add(100*time.Millisecond))

	// Now a sustained backlog: standing 5000, write 800 every 100ms, 800 drains.
	for i := 1; i <= 5; i++ {
		feed(800, 5800, t0.Add(time.Duration(100+i*100)*time.Millisecond))
	}
	queryTs := t0.Add(700 * time.Millisecond)

	rOff, okOff := off.Bitrate(queryTs)
	rOn, okOn := on.Bitrate(queryTs)
	require.True(t, okOff)
	require.True(t, okOn)
	require.Greater(t, rOff, rOn, "busy-only over-estimates by counting idle-drained bytes")
}

// Wall-clock mode has no notion of backlog (buffered is always reported as 0):
// it measures throughput over elapsed wall-clock time and counts all bytes.
func TestBitrateCalculatorWallClock(t *testing.T) {
	c := NewBitrateCalculator(BitrateDuration, BitrateWindow, BitrateModeWallClock)

	t0 := time.Now()
	// 1000 bytes every 100ms with no observable buffer = 10000 B/s = 80000 bps.
	for i := 0; i < 20; i++ {
		c.AddBytes(1000, 0, t0.Add(time.Duration(i)*100*time.Millisecond))
	}

	bitrate, ok := c.Bitrate(t0.Add(2 * time.Second))
	require.True(t, ok)
	require.InDelta(t, 80000, bitrate, 8000)
}
