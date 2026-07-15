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
	// BitrateModeBusyOnly measures the drain rate over backlogged ("busy") time
	// only -- idle gaps between sparse writes are excluded from the denominator,
	// so a fast channel fed sparsely is not mistaken for a slow one. All bytes
	// that drained are counted in the numerator. In a mixed window (idle drain
	// followed by a backlog) this can transiently over-estimate the rate
	// (bounded by the window); the bias errs towards not dropping and preserves
	// the reliable path's burst-credit retry behaviour. This is the default for
	// the data channel writers.
	BitrateModeBusyOnly BitrateMode = iota

	// BitrateModeExcludeIdleDrain is like BitrateModeBusyOnly but additionally
	// drops bytes that drained across a NON-backlogged interval (the buffer
	// emptied and the link went idle). The result reflects only intervals where
	// the buffer stayed backlogged the whole time -- a clean drain-capacity
	// estimate with no idle contamination.
	BitrateModeExcludeIdleDrain

	// BitrateModeWallClock measures throughput over elapsed wall-clock time and
	// counts all bytes. For consumers that have no send buffer to observe (e.g.
	// a receiver measuring its own incoming rate), where backlog has no meaning.
	BitrateModeWallClock
)

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
// governs the numerator (which drained bytes count) and denominator (busy time
// vs wall clock).
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

	// Count the interval since the previous sample towards the rate denominator
	// only if the channel was backlogged for the whole interval, i.e. it was
	// actively draining rather than sitting idle. Idle gaps must be excluded:
	// otherwise a fast channel fed sparsely reports its (low) offered load
	// instead of its drain capacity. When no backlog is ever observed no busy
	// time accrues and Bitrate() reports "no estimate" (ok == false) rather
	// than a confidently-low number that would throttle bursts.
	//
	// The right signal is the buffer occupancy *just before* this write: no
	// bytes are added between writes, so the buffer only drains across an
	// interval and its pre-write level is the interval minimum -- if that is
	// > 0 the buffer never emptied and the full interval was busy. bufferedAmout
	// is sampled right after the write, so it includes the bytes we just
	// enqueued (not yet SACKed); subtract them to recover the pre-write level.
	//
	// NOTE: gating on the post-write buffered amount would be wrong -- right
	// after any write the buffer holds the just-enqueued bytes, so it is ~never
	// zero and every idle gap would be counted, collapsing the estimate back to
	// wall-clock throughput.
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
		// The buffer emptied during this interval (the link went idle), so the
		// bytes that drained are not clean drain-capacity evidence -- the drain
		// finished at an unknown point in the interval. Drop them so they don't
		// inflate the rate. (In busy-only mode they are kept.)
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
	// In busy modes the denominator is backlogged (busy) time only, not wall
	// clock, so idle gaps don't dilute the estimate of the channel's drain
	// capacity. In wall-clock mode it is the elapsed window.
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
