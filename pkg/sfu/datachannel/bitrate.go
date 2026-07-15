package datachannel

import (
	"sync"
	"time"

	"github.com/gammazero/deque"

	"github.com/livekit/protocol/utils/mono"
)

const (
	BitrateDuration = 2 * time.Second
	BitrateWindow   = 100 * time.Millisecond
)

// BitrateMode selects how the sliding-window rate is computed.
type BitrateMode int

const (
	// BitrateModeBusyOnly divides drained bytes by backlogged ("busy") time
	// only; idle gaps are excluded from the denominator. All drained bytes count.
	BitrateModeBusyOnly BitrateMode = iota

	// BitrateModeExcludeIdleDrain is like BitrateModeBusyOnly but also excludes
	// bytes drained across a non-backlogged interval from the numerator.
	BitrateModeExcludeIdleDrain

	// BitrateModeWallClock divides all bytes by elapsed wall-clock time. For
	// consumers with no send buffer, e.g. a receiver measuring its incoming rate.
	BitrateModeWallClock
)

func (m BitrateMode) String() string {
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

// BitrateCalculator calculates bitrate over sliding window
type BitrateCalculator struct {
	lock           sync.Mutex
	windowDuration time.Duration
	duration       time.Duration

	windows deque.Deque[bitrateWindow]
	active  bitrateWindow

	bytes              int
	busy               time.Duration
	lastBufferedAmount int
	last               time.Time
	start              time.Time
	mode               BitrateMode
}

// NewBitrateCalculator creates a calculator. See BitrateMode for how mode
// governs the numerator and denominator.
func NewBitrateCalculator(duration time.Duration, window time.Duration, mode BitrateMode) *BitrateCalculator {
	windowCnt := int((duration + (window - 1)) / window)
	if windowCnt == 0 {
		windowCnt = 1
	}
	now := mono.Now()
	c := &BitrateCalculator{
		duration:       duration,
		windowDuration: window,
		start:          now,
		active:         bitrateWindow{start: now},
		mode:           mode,
	}
	c.windows.SetBaseCap(windowCnt + 1)

	return c
}

func (c *BitrateCalculator) AddBytes(bytes int, bufferedAmout int, ts time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()

	// bufferedAmout is sampled right after the write, so it includes the bytes
	// just enqueued; subtracting them gives the buffer occupancy before the
	// write. Between writes the buffer only drains, so a non-zero pre-write
	// level means it stayed backlogged for the whole interval.
	backlogged := bufferedAmout-bytes > 0
	var busy time.Duration
	if !c.last.IsZero() && backlogged {
		if busy = ts.Sub(c.last); busy < 0 {
			busy = 0
		}
	}
	c.last = ts

	// bytes that actually drained since the previous sample
	bytes -= bufferedAmout - c.lastBufferedAmount
	if bytes < 0 {
		// it is possible that internal buffering (non-data like DCEP packet from webrtc) caused bytes to be negative
		bytes = 0
	}
	c.lastBufferedAmount = bufferedAmout

	if c.mode == BitrateModeExcludeIdleDrain && !backlogged {
		// drain across a non-backlogged interval finished at an unknown point,
		// so it is not a reliable rate sample
		bytes = 0
	}

	if ts.Sub(c.active.start) >= c.windowDuration {
		c.windows.PushBack(c.active)
		c.active.start = ts
		c.active.bytes = 0
		c.active.busy = 0

		for c.windows.Len() > 0 {
			// pop expired windows
			if w := c.windows.Front(); ts.Sub(w.start) > (c.duration + c.windowDuration) {
				c.bytes -= w.bytes
				c.busy -= w.busy
				c.windows.PopFront()
			} else {
				c.start = w.start
				break
			}
		}
		if c.windows.Len() == 0 {
			c.start = ts
			c.bytes = 0
			c.busy = 0
		}
	}
	c.bytes += bytes
	c.active.bytes += bytes
	c.busy += busy
	c.active.busy += busy
}

func (c *BitrateCalculator) Bitrate(ts time.Time) (int, bool) {
	return c.bitrate(ts, false)
}

func (c *BitrateCalculator) ForceBitrate(ts time.Time) (int, bool) {
	return c.bitrate(ts, true)
}

func (c *BitrateCalculator) bitrate(ts time.Time, force bool) (int, bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	// busy modes divide by backlogged time; wall-clock mode by elapsed time
	denom := c.busy
	if c.mode == BitrateModeWallClock {
		denom = ts.Sub(c.start)
	}
	if denom < c.windowDuration {
		if force {
			denom = c.windowDuration
		} else {
			return 0, false
		}
	}

	return c.bytes * 8 * 1000 / int(denom.Milliseconds()), true
}

type bitrateWindow struct {
	start time.Time
	bytes int
	busy  time.Duration
}
