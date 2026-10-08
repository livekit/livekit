package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/livekit"
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

	t.Run("logging a nil worker does not panic", func(t *testing.T) {
		var w *StatsWorker
		require.NoError(t, w.MarshalLogObject(zapcore.NewMapObjectEncoder()))
	})
}

func TestGetOrCreateWorkerReleasedGuard(t *testing.T) {
	// ParticipantActive overtaken by the participant's close arrives with a guard that
	// ParticipantLeft already released. It must not replace the closed worker with one
	// nothing can release.
	ts := &telemetryService{workers: make(map[statsWorkerKey]*StatsWorker)}
	roomID, pID := livekit.RoomID("room"), livekit.ParticipantID("participant")

	var g ReferenceGuard
	w, found := ts.getOrCreateWorker(context.Background(), roomID, "", pID, "", &g)
	require.False(t, found)
	require.True(t, w.Close(&g))

	t.Run("closed worker still in the map", func(t *testing.T) {
		late, found := ts.getOrCreateWorker(context.Background(), roomID, "", pID, "", &g)
		require.True(t, found)
		require.Same(t, w, late)
		require.Same(t, w, ts.workers[statsWorkerKey{roomID, pID}])
	})

	t.Run("closed worker already reaped", func(t *testing.T) {
		delete(ts.workers, statsWorkerKey{roomID, pID})

		late, found := ts.getOrCreateWorker(context.Background(), roomID, "", pID, "", &g)
		require.True(t, found)
		require.Nil(t, late)
		require.Empty(t, ts.workers)
		late.SetConnected()
	})
}
