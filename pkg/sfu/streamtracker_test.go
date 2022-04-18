package sfu

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/testutils"
	"github.com/livekit/protocol/logger"
)

func newStreamTracker(samplesRequired uint32, cyclesRequired uint32, cycleDuration time.Duration) *StreamTracker {
	return NewStreamTracker(StreamTrackerParams{
		SamplesRequired: samplesRequired,
		CyclesRequired:  cyclesRequired,
		CycleDuration:   cycleDuration,
		Logger:          logger.Logger(logger.GetLogger()),
	})
}

func TestStreamTracker(t *testing.T) {
	t.Run("flips to active on first observe", func(t *testing.T) {
		callbackCalled := atomic.NewBool(false)
		tracker := newStreamTracker(5, 60, 500*time.Millisecond)
		tracker.Start()
		tracker.OnStatusChanged(func(status StreamStatus) {
			callbackCalled.Store(true)
		})
		require.Equal(t, StreamStatusStopped, tracker.Status())

		// observe first packet
		tracker.Observe(1, 0, 0, 0)

		testutils.WithTimeout(t, func() string {
			if callbackCalled.Load() {
				return ""
			} else {
				return "first packet didn't activate stream"
			}
		})

		require.Equal(t, StreamStatusActive, tracker.Status())
		require.True(t, callbackCalled.Load())

		tracker.Stop()
	})

	t.Run("flips to inactive immediately", func(t *testing.T) {
		tracker := newStreamTracker(5, 60, 500*time.Millisecond)
		tracker.Start()
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(1, 0, 0, 0)
		testutils.WithTimeout(t, func() string {
			if tracker.Status() == StreamStatusActive {
				return ""
			} else {
				return "first packet did not activate stream"
			}
		})

		callbackCalled := atomic.NewBool(false)
		tracker.OnStatusChanged(func(status StreamStatus) {
			callbackCalled.Store(true)
		})
		require.Equal(t, StreamStatusActive, tracker.Status())

		// run a single iteration
		tracker.detectChanges()

		testutils.WithTimeout(t, func() string {
			if callbackCalled.Load() {
				return ""
			} else {
				return "inactive cycle did not declare stream stopped"
			}
		})
		require.Equal(t, StreamStatusStopped, tracker.Status())
		require.True(t, callbackCalled.Load())

		tracker.Stop()
	})

	t.Run("flips back to active after iterations", func(t *testing.T) {
		tracker := newStreamTracker(1, 2, 500*time.Millisecond)
		tracker.Start()
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(1, 0, 0, 0)
		testutils.WithTimeout(t, func() string {
			if tracker.Status() == StreamStatusActive {
				return ""
			} else {
				return "first packet did not activate stream"
			}
		})

		tracker.maybeSetStatus(StreamStatusStopped)

		tracker.Observe(2, 0, 0, 0)
		tracker.detectChanges()
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(3, 0, 0, 0)
		tracker.detectChanges()
		require.Equal(t, StreamStatusActive, tracker.Status())

		tracker.Stop()
	})

	t.Run("changes to inactive when paused", func(t *testing.T) {
		tracker := newStreamTracker(5, 60, 500*time.Millisecond)
		tracker.Start()
		tracker.Observe(1, 0, 0, 0)
		testutils.WithTimeout(t, func() string {
			if tracker.Status() == StreamStatusActive {
				return ""
			} else {
				return "first packet did not activate stream"
			}
		})

		tracker.SetPaused(true)
		tracker.detectChanges()
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Stop()
	})

	t.Run("flips back to active on first observe after reset", func(t *testing.T) {
		callbackCalled := atomic.NewUint32(0)
		tracker := newStreamTracker(5, 60, 500*time.Millisecond)
		tracker.Start()
		tracker.OnStatusChanged(func(status StreamStatus) {
			callbackCalled.Inc()
		})
		require.Equal(t, StreamStatusStopped, tracker.Status())

		// observe first packet
		tracker.Observe(1, 0, 0, 0)

		testutils.WithTimeout(t, func() string {
			if callbackCalled.Load() == 1 {
				return ""
			} else {
				return fmt.Sprintf("expected onStatusChanged to be called once, actual: %d", callbackCalled.Load())
			}
		})

		require.Equal(t, StreamStatusActive, tracker.Status())
		require.Equal(t, uint32(1), callbackCalled.Load())

		// observe a few more
		tracker.Observe(2, 0, 0, 0)
		tracker.Observe(3, 0, 0, 0)
		tracker.Observe(4, 0, 0, 0)
		tracker.Observe(5, 0, 0, 0)
		tracker.detectChanges()

		// should still be active
		require.Equal(t, StreamStatusActive, tracker.Status())

		// Reset. The first packet after reset should flip state again
		tracker.Reset()
		require.Equal(t, StreamStatusStopped, tracker.Status())

		// first packet after reset
		tracker.Observe(1, 0, 0, 0)

		testutils.WithTimeout(t, func() string {
			if callbackCalled.Load() == 2 {
				return ""
			} else {
				return fmt.Sprintf("expected onStatusChanged to be called twice, actual %d", callbackCalled.Load())
			}
		})

		require.Equal(t, StreamStatusActive, tracker.Status())
		require.Equal(t, uint32(2), callbackCalled.Load())

		tracker.Stop()
	})
}
