package streamallocator

import (
	"fmt"
	"time"

	"github.com/livekit/protocol/logger"
)

// ------------------------------------------------

type NackTrackerParams struct {
	Name              string
	Logger            logger.Logger
	WindowMinDuration time.Duration
	WindowMaxDuration time.Duration
	RatioThreshold    float64
}

type NackTracker struct {
	params NackTrackerParams

	windowStartTime time.Time
	packets         uint32
	repeatedNacks   uint32
}

func NewNackTracker(params NackTrackerParams) *NackTracker {
	return &NackTracker{
		params: params,
	}
}

func (n *NackTracker) Add(packets uint32, repeatedNacks uint32) {
	if n.params.WindowMaxDuration != 0 && !n.windowStartTime.IsZero() && time.Since(n.windowStartTime) > n.params.WindowMaxDuration {
		n.windowStartTime = time.Time{}
		n.packets = 0
		n.repeatedNacks = 0
	}

	//
	// Start NACK monitoring window only when a repeated NACK happens.
	// This allows locking tightly to when NACKs start happening and
	// check if the NACKs keep adding up (potentially a sign of congestion)
	// or isolated losses
	//
	if n.repeatedNacks == 0 && repeatedNacks != 0 {
		n.windowStartTime = time.Now()
	}

	if !n.windowStartTime.IsZero() {
		n.packets += packets
		n.repeatedNacks += repeatedNacks
	}
}

func (n *NackTracker) GetRatio() float64 {
	ratio := 0.0
	if n.packets != 0 {
		ratio = float64(n.repeatedNacks) / float64(n.packets)
		if ratio > 1.0 {
			ratio = 1.0
		}
	}

	return ratio
}

func (n *NackTracker) IsTriggered() bool {
	if n.params.WindowMinDuration != 0 && !n.windowStartTime.IsZero() && time.Since(n.windowStartTime) > n.params.WindowMinDuration {
		return n.GetRatio() > n.params.RatioThreshold
	}

	return false
}

func (n *NackTracker) ToString() string {
	window := ""
	if !n.windowStartTime.IsZero() {
		window = fmt.Sprintf("t: %+v|%+v", n.windowStartTime, time.Since(n.windowStartTime))
	}
	return fmt.Sprintf("n: %s, t: %s, p: %d,  rn: %d, rn/p: %f", n.params.Name, window, n.packets, n.repeatedNacks, n.GetRatio())
}

// ------------------------------------------------
