// Copyright 2026 Hideout
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package telemetry

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
)

// analyticsTestDSNEnv points the store tests at a throwaway Postgres. Without it the
// database-backed tests are skipped, matching how the other docker-backed tests in
// this repo behave.
const analyticsTestDSNEnv = "LIVEKIT_TEST_ANALYTICS_POSTGRES_DSN"

func TestMigrationStatementsQuoteTheConfiguredSchema(t *testing.T) {
	store := &pgAnalyticsStore{
		schema: "livekit_analytics",
		target: pgx.Identifier{"livekit_analytics", roomByteSamplesTable},
	}

	statements := store.migrationStatements()
	require.Len(t, statements, 4)
	require.Equal(t, `CREATE SCHEMA IF NOT EXISTS "livekit_analytics"`, statements[0])
	for _, stmt := range statements[1:] {
		require.Contains(t, stmt, `"livekit_analytics"."room_byte_samples"`)
	}
	require.Contains(t, statements[1], "direction IN ('upstream', 'downstream')")
}

func TestRoomByteSampleCopyRowMatchesColumnOrder(t *testing.T) {
	sampledAt := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	sample := roomByteSample{
		RoomName:        "world-org-1",
		RoomID:          "RM_abc",
		ParticipantID:   "PA_1",
		TrackID:         "TR_1",
		Direction:       directionUpstream,
		PrimaryBytes:    1,
		RetransmitBytes: 2,
		PaddingBytes:    3,
		SampledAt:       sampledAt,
		NodeID:          "ND_1",
	}

	row := sample.copyRow()
	require.Len(t, row, len(roomByteSampleColumns))
	require.Equal(t, []any{
		"world-org-1", "RM_abc", "PA_1", "TR_1", directionUpstream,
		int64(1), int64(2), int64(3), sampledAt, "ND_1",
	}, row)
}

func TestIsConcurrentDDLError(t *testing.T) {
	require.True(t, isConcurrentDDLError(&pgconn.PgError{Code: "42P07"}))
	require.True(t, isConcurrentDDLError(&pgconn.PgError{Code: "23505"}))
	require.False(t, isConcurrentDDLError(&pgconn.PgError{Code: "42501"})) // insufficient_privilege
	require.False(t, isConcurrentDDLError(errors.New("connection refused")))
}

// TestStoreWritesSamples exercises the real migration and COPY path. It only runs
// when a disposable Postgres is provided; the schema it creates is dropped again.
func TestStoreWritesSamples(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(analyticsTestDSNEnv))
	if dsn == "" {
		t.Skipf("set %s to run the analytics store integration test", analyticsTestDSNEnv)
	}

	conf, err := config.PostgresAnalyticsConfig{DSN: dsn, Schema: "livekit_analytics_test"}.Resolved()
	require.NoError(t, err)

	store, err := newPgAnalyticsStore(conf)
	require.NoError(t, err)
	// registered first, so it runs last: the schema is dropped while the pool is open
	t.Cleanup(store.close)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, err := store.pool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "livekit_analytics_test" CASCADE`)
		require.NoError(t, err)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.NoError(t, store.migrate(ctx))
	// migrations are idempotent, every node runs them at startup
	require.NoError(t, store.migrate(ctx))

	sampledAt := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, store.insert(ctx, []roomByteSample{
		{
			RoomName: "world-org-1", RoomID: "RM_a", ParticipantID: "PA_1", TrackID: "TR_1",
			Direction: directionUpstream, PrimaryBytes: 100, RetransmitBytes: 10, PaddingBytes: 1,
			SampledAt: sampledAt, NodeID: "ND_1",
		},
		{
			RoomName: "world-org-1", RoomID: "RM_a", ParticipantID: "PA_2", TrackID: "TR_2",
			Direction: directionDownstream, PrimaryBytes: 200, RetransmitBytes: 0, PaddingBytes: 0,
			SampledAt: sampledAt, NodeID: "ND_1",
		},
	}))

	var rows, totalBytes int64
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(bytes), 0) FROM "livekit_analytics_test"."room_byte_samples" WHERE room_name = $1`,
		"world-org-1",
	).Scan(&rows, &totalBytes))
	require.EqualValues(t, 2, rows)
	require.EqualValues(t, 311, totalBytes)
}
