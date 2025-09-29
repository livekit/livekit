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
		w.Close(&g0)
		require.False(t, w.Closed(&g1))
		w.Close(&g1)
		require.True(t, w.Closed(&g1))
	})
}
