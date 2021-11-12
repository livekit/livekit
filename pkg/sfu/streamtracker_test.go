package sfu

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStreamTracker(t *testing.T) {
	t.Run("flips to inactive immediately", func(t *testing.T) {
		callbackCalled := atomicBool(0)
		tracker := NewStreamTracker()
		tracker.OnStatusChanged = func(status StreamStatus) {
			callbackCalled.set(true)
		}
		require.Equal(t, StreamStatusActive, tracker.Status())

		// run a single interation
		tracker.detectChanges()
		require.Equal(t, StreamStatusStopped, tracker.Status())
		require.True(t, callbackCalled.get())
	})

	t.Run("flips back to active after iterations", func(t *testing.T) {
		tracker := NewStreamTracker()
		tracker.CyclesRequired = 2
		tracker.SamplesRequired = 1
		tracker.setStatus(StreamStatusStopped)

		tracker.Observe(1)
		tracker.detectChanges()
		require.Equal(t, StreamStatusStopped, tracker.Status())

		tracker.Observe(2)
		tracker.detectChanges()
		require.Equal(t, StreamStatusActive, tracker.Status())
	})

	t.Run("does not change to inactive when paused", func(t *testing.T) {
		tracker := NewStreamTracker()
		tracker.SetPaused(true)
		tracker.detectChanges()
		require.Equal(t, StreamStatusActive, tracker.Status())
	})
}
