package sfu

import (
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/testutils"

	"github.com/stretchr/testify/require"
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

		testutils.WithTimeout(t, "first packet makes stream active", func() bool {
			return callbackCalled.get()
		})

		require.Equal(t, StreamStatusActive, tracker.Status())
		require.True(t, callbackCalled.get())
	})

	t.Run("flips to inactive immediately", func(t *testing.T) {
		tracker := NewStreamTracker(5, 60, 500*time.Millisecond)
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(1)
		testutils.WithTimeout(t, "first packet makes stream active", func() bool {
			return tracker.Status() == StreamStatusActive
		})

		callbackCalled := atomicBool(0)
		tracker.OnStatusChanged(func(status StreamStatus) {
			callbackCalled.set(true)
		})
		require.Equal(t, StreamStatusActive, tracker.Status())

		// run a single interation
		tracker.detectChanges()
		require.Equal(t, StreamStatusStopped, tracker.Status())
		require.True(t, callbackCalled.get())
	})

	t.Run("flips back to active after iterations", func(t *testing.T) {
		tracker := NewStreamTracker(1, 2, 500*time.Millisecond)
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(1)
		testutils.WithTimeout(t, "first packet makes stream active", func() bool {
			return tracker.Status() == StreamStatusActive
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
		testutils.WithTimeout(t, "first packet makes stream active", func() bool {
			return tracker.Status() == StreamStatusActive
		})

		tracker.SetPaused(true)
		tracker.detectChanges()
		require.Equal(t, StreamStatusActive, tracker.Status())
	})
}
