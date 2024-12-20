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

// BitrateCalculator calculates bitrate over sliding window
type BitrateCalculator struct {
	lock           sync.Mutex
	windowDuration time.Duration
	duration       time.Duration

	windows deque.Deque[bitrateWindow]
	active  bitrateWindow

	bytes              int
	lastBufferedAmount int
	start              time.Time
}

func NewBitrateCalculator(duration time.Duration, window time.Duration) *BitrateCalculator {
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
	}
	c.windows.SetBaseCap(windowCnt + 1)

	return c
}

func (c *BitrateCalculator) AddBytes(bytes int, bufferedAmout int, ts time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()

	bytes -= bufferedAmout - c.lastBufferedAmount
	c.lastBufferedAmount = bufferedAmout
	if ts.Sub(c.active.start) >= c.windowDuration {
		c.windows.PushBack(c.active)
		c.active.start = ts
		c.active.bytes = 0

		for c.windows.Len() > 0 {
			// pop expired windows
			if w := c.windows.Front(); ts.Sub(w.start) > (c.duration + c.windowDuration) {
				c.bytes -= w.bytes
				c.windows.PopFront()
			} else {
				c.start = w.start
				break
			}
		}
		if c.windows.Len() == 0 {
			c.start = ts
			c.bytes = 0
		}
	}
	c.bytes += bytes
	c.active.bytes += bytes

}

func (c *BitrateCalculator) Bitrate(ts time.Time) int {
	c.lock.Lock()
	defer c.lock.Unlock()
	duration := ts.Sub(c.start)
	if duration < c.windowDuration {
		duration = c.windowDuration
	}

	return c.bytes * 8 * 1000 / int(duration.Milliseconds())
}

type bitrateWindow struct {
	start time.Time
	bytes int
}
