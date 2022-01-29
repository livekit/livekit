package sfu

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/testutils"
)

func TestStreamTracker(t *testing.T) {
	t.Run("flips to active on first observe", func(t *testing.T) {
		callbackCalled := atomicBool(0)
		tracker := NewStreamTracker(5, 60, 500*time.Millisecond)
		tracker.OnStatusChanged(func(status StreamStatus) {
			callbackCalled.set(true)
		})
		require.Equal(t, StreamStatusStopped, tracker.Status())

		// observe first packet
		tracker.Observe(1)

		testutils.WithTimeout(t, func() string {
			if callbackCalled.get() {
				return ""
			} else {
				return "first packet didn't activate stream"
			}
		})

		require.Equal(t, StreamStatusActive, tracker.Status())
		require.True(t, callbackCalled.get())
	})

	t.Run("flips to inactive immediately", func(t *testing.T) {
		tracker := NewStreamTracker(5, 60, 500*time.Millisecond)
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(1)
		testutils.WithTimeout(t, func() string {
			if tracker.Status() == StreamStatusActive {
				return ""
			} else {
				return "first packet did not activate stream"
			}
		})

		callbackCalled := atomicBool(0)
		tracker.OnStatusChanged(func(status StreamStatus) {
			callbackCalled.set(true)
		})
		require.Equal(t, StreamStatusActive, tracker.Status())

		// run a single iteration
		tracker.detectChanges()
		require.Equal(t, StreamStatusStopped, tracker.Status())
		require.True(t, callbackCalled.get())
	})

	t.Run("flips back to active after iterations", func(t *testing.T) {
		tracker := NewStreamTracker(1, 2, 500*time.Millisecond)
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(1)
		testutils.WithTimeout(t, func() string {
			if tracker.Status() == StreamStatusActive {
				return ""
			} else {
				return "first packet did not activate stream"
			}
		})

		tracker.maybeSetStopped()

		tracker.Observe(2)
		tracker.detectChanges()
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(3)
		tracker.detectChanges()
		require.Equal(t, StreamStatusActive, tracker.Status())
	})

	t.Run("does not change to inactive when paused", func(t *testing.T) {
		tracker := NewStreamTracker(5, 60, 500*time.Millisecond)
		tracker.Observe(1)
		testutils.WithTimeout(t, func() string {
			if tracker.Status() == StreamStatusActive {
				return ""
			} else {
				return "first packet did not activate stream"
			}
		})

		tracker.SetPaused(true)
		tracker.detectChanges()
		require.Equal(t, StreamStatusActive, tracker.Status())
	})

	t.Run("flips back to active on first observe after reset", func(t *testing.T) {
		callbackCalled := atomicUint32(0)
		tracker := NewStreamTracker(5, 60, 500*time.Millisecond)
		tracker.OnStatusChanged(func(status StreamStatus) {
			callbackCalled.add(1)
		})
		require.Equal(t, StreamStatusStopped, tracker.Status())

		// observe first packet
		tracker.Observe(1)

		testutils.WithTimeout(t, func() string {
			if callbackCalled.get() == 1 {
				return ""
			} else {
				return fmt.Sprintf("expected onStatusChanged to be called once, actual: %d", callbackCalled.get())
			}
		})

		require.Equal(t, StreamStatusActive, tracker.Status())
		require.Equal(t, uint32(1), callbackCalled.get())

		// observe a few more
		tracker.Observe(2)
		tracker.Observe(3)
		tracker.Observe(4)
		tracker.Observe(5)
		tracker.detectChanges()

		// should still be active
		require.Equal(t, StreamStatusActive, tracker.Status())

		// Reset. The first packet after reset should flip state again
		tracker.Reset()
		require.Equal(t, StreamStatusStopped, tracker.Status())

		// first packet after reset
		tracker.Observe(1)

		testutils.WithTimeout(t, func() string {
			if callbackCalled.get() == 2 {
				return ""
			} else {
				return fmt.Sprintf("expected onStatusChanged to be called twice, actual %d", callbackCalled.get())
			}
		})

		require.Equal(t, StreamStatusActive, tracker.Status())
		require.Equal(t, uint32(2), callbackCalled.get())
	})
}
