package telemetry

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatsWorker(t *testing.T) {
	t.Run("reference counted close works", func(t *testing.T) {
		var g0, g1 ReferenceGuard
		w := newStatsWorker(t.Context(), nil, "", "", "", "", &g0)
		require.False(t, w.Closed(&g1))
		require.False(t, w.Close(&g0))
		require.False(t, w.Closed(&g1))
		require.True(t, w.Close(&g1))
		require.True(t, w.Closed(&g1))
	})

	// a ReferenceGuard records that it activated some worker, not which one, so a
	// superseded worker has to hand its references to the one reachable in its place
	t.Run("force close hands references to the successor", func(t *testing.T) {
		t.Run("a guard shared by both workers", func(t *testing.T) {
			// the second worker never got a reference, the guard was already activated
			var g ReferenceGuard
			superseded := newStatsWorker(t.Context(), nil, "", "", "", "", &g)
			survivor := newStatsWorker(t.Context(), nil, "", "", "", "", &g)
			require.Equal(t, 1, superseded.refCount.count)
			require.Equal(t, 0, survivor.refCount.count)

			require.True(t, superseded.ForceClose(survivor))
			require.Equal(t, 0, superseded.refCount.count)
			require.Equal(t, 1, survivor.refCount.count)

			// without the hand over this would leave the survivor at -1 and never closed
			require.True(t, survivor.Close(&g))
			require.True(t, survivor.Closed(&g))
		})

		t.Run("a guard per worker", func(t *testing.T) {
			var gSuperseded, gSurvivor ReferenceGuard
			superseded := newStatsWorker(t.Context(), nil, "", "", "", "", &gSuperseded)
			survivor := newStatsWorker(t.Context(), nil, "", "", "", "", &gSurvivor)

			require.True(t, superseded.ForceClose(survivor))
			require.Equal(t, 2, survivor.refCount.count)

			// the superseded worker's owner departs, it must not close the survivor early
			require.False(t, survivor.Close(&gSuperseded))
			require.True(t, survivor.Close(&gSurvivor))
		})

		t.Run("closing an already closed worker holds on to its references", func(t *testing.T) {
			var g ReferenceGuard
			superseded := newStatsWorker(t.Context(), nil, "", "", "", "", &g)
			survivor := newStatsWorker(t.Context(), nil, "", "", "", "", nil)

			require.True(t, superseded.ForceClose(nil))
			require.False(t, superseded.ForceClose(survivor))
			require.Equal(t, 0, survivor.refCount.count)
		})
	})
}
