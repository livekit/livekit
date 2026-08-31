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

	steps := store.migrationStatements()
	require.Len(t, steps, 6)
	require.Equal(t, `CREATE SCHEMA IF NOT EXISTS "livekit_analytics"`, steps[0].stmt)
	for _, s := range steps[1:] {
		require.Contains(t, s.stmt, `"livekit_analytics"."room_byte_samples"`)
	}
	require.Contains(t, steps[1].stmt, "direction IN ('upstream', 'downstream')")

	// no server has run this schema yet, so every column lives directly in
	// CREATE TABLE - there is no deployed table an ADD COLUMN step would be
	// upgrading in place. org_id/participant_kind/packet counts arrived alongside
	// bytes for the same reason; packets mirrors bytes, a generated total so a
	// rollup never has to remember to sum all three packet columns itself.
	for _, col := range []string{"org_id", "participant_kind", "primary_packets", "retransmit_packets", "padding_packets"} {
		require.Contains(t, steps[1].stmt, col)
	}
	require.Contains(t, steps[1].stmt, "bytes               bigint      NOT NULL GENERATED ALWAYS AS (primary_bytes + retransmit_bytes + padding_bytes) STORED")
	require.Contains(t, steps[1].stmt, "packets             bigint      GENERATED ALWAYS AS (primary_packets + retransmit_packets + padding_packets) STORED")

	// every step races the same way across nodes except the unique constraint,
	// which must not treat a real duplicate-data violation as a race
	for i, s := range steps {
		if strings.Contains(s.stmt, "ADD CONSTRAINT") {
			continue
		}
		require.Truef(t, mapsEqual(s.raceCodes, concurrentDDLCodes), "step %d: %s", i, s.stmt)
	}

	constraintStep := steps[2]
	require.Contains(t, constraintStep.stmt, "ADD CONSTRAINT")
	require.Contains(t, constraintStep.stmt, "UNIQUE (node_id, room_id, participant_id, track_id, direction, sampled_at)")
	require.True(t, mapsEqual(constraintStep.raceCodes, constraintRaceCodes))
	require.NotContains(t, constraintStep.raceCodes, "23505",
		"a real duplicate in existing data must fail the migration, not be swallowed as a race")
}

func mapsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func TestRoomByteSampleCopyRowMatchesColumnOrder(t *testing.T) {
	sampledAt := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	sample := roomByteSample{
		OrgID:             "org_01HQZX",
		RoomName:          "world-org-1",
		RoomID:            "RM_abc",
		ParticipantID:     "PA_1",
		ParticipantKind:   "STANDARD",
		TrackID:           "TR_1",
		Direction:         directionUpstream,
		PrimaryBytes:      1,
		RetransmitBytes:   2,
		PaddingBytes:      3,
		PrimaryPackets:    4,
		RetransmitPackets: 5,
		PaddingPackets:    6,
		SampledAt:         sampledAt,
		NodeID:            "ND_1",
	}

	row := sample.copyRow()
	require.Len(t, row, len(roomByteSampleColumns))
	require.Equal(t, []any{
		"org_01HQZX", "world-org-1", "RM_abc", "PA_1", "STANDARD", "TR_1", directionUpstream,
		int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), sampledAt, "ND_1",
	}, row)
}

// An unresolved organization or participant kind must reach Postgres as NULL, not
// as an empty string that would silently join to nothing and group with itself at
// rollup time.
func TestRoomByteSampleCopyRowWritesUnknownOrgAndKindAsNull(t *testing.T) {
	row := roomByteSample{RoomName: "world-org-1"}.copyRow()
	require.Len(t, row, len(roomByteSampleColumns))

	require.Equal(t, "org_id", roomByteSampleColumns[0])
	require.Nil(t, row[0])

	require.Equal(t, "participant_kind", roomByteSampleColumns[4])
	require.Nil(t, row[4])
}

func TestIsRaceError(t *testing.T) {
	require.True(t, isRaceError(&pgconn.PgError{Code: "42P07"}, concurrentDDLCodes))
	require.True(t, isRaceError(&pgconn.PgError{Code: "42701"}, concurrentDDLCodes))
	require.True(t, isRaceError(&pgconn.PgError{Code: "23505"}, concurrentDDLCodes))
	require.False(t, isRaceError(&pgconn.PgError{Code: "42501"}, concurrentDDLCodes)) // insufficient_privilege
	require.False(t, isRaceError(errors.New("connection refused"), concurrentDDLCodes))

	// the narrower set used for the unique constraint: a race with another node (or
	// a second migrate() call re-adding the same named constraint) is still
	// tolerated, but a genuine duplicate-data violation is not
	require.True(t, isRaceError(&pgconn.PgError{Code: "42P07"}, constraintRaceCodes))
	require.False(t, isRaceError(&pgconn.PgError{Code: "23505"}, constraintRaceCodes))
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
			OrgID: "org_01HQZX", RoomName: "world-org-1", RoomID: "RM_a", ParticipantID: "PA_1",
			ParticipantKind: "STANDARD", TrackID: "TR_1",
			Direction: directionUpstream, PrimaryBytes: 100, RetransmitBytes: 10, PaddingBytes: 1,
			PrimaryPackets: 9, RetransmitPackets: 2, PaddingPackets: 0,
			SampledAt: sampledAt, NodeID: "ND_1",
		},
		{
			OrgID: "org_01HQZX", RoomName: "world-org-1", RoomID: "RM_a", ParticipantID: "PA_2",
			ParticipantKind: "STANDARD", TrackID: "TR_2",
			Direction: directionDownstream, PrimaryBytes: 200, RetransmitBytes: 0, PaddingBytes: 0,
			PrimaryPackets: 15, RetransmitPackets: 0, PaddingPackets: 0,
			SampledAt: sampledAt, NodeID: "ND_1",
		},
		{
			// no organization resolved: still recorded, still billable bytes
			RoomName: "world-org-1", RoomID: "RM_a", ParticipantID: "PA_3", TrackID: "TR_3",
			Direction: directionDownstream, PrimaryBytes: 7, RetransmitBytes: 0, PaddingBytes: 0,
			SampledAt: sampledAt, NodeID: "ND_1",
		},
		{
			// an egress participant: recorded and billable exactly like any other
			// row, but the column now exists for a rollup to exclude it by name
			OrgID: "org_01HQZX", RoomName: "world-org-1", RoomID: "RM_a", ParticipantID: "PA_egress",
			ParticipantKind: "EGRESS", TrackID: "TR_4",
			Direction: directionDownstream, PrimaryBytes: 50, RetransmitBytes: 0, PaddingBytes: 0,
			SampledAt: sampledAt, NodeID: "ND_1",
		},
	}))

	var rows, totalBytes int64
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(bytes), 0) FROM "livekit_analytics_test"."room_byte_samples" WHERE room_name = $1`,
		"world-org-1",
	).Scan(&rows, &totalBytes))
	require.EqualValues(t, 4, rows)
	require.EqualValues(t, 368, totalBytes)

	// usage rolls up by organization, and the unresolved row is visibly separate
	// rather than folded into an organization it does not belong to
	var orgBytes, unattributedRows int64
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT coalesce(sum(bytes) FILTER (WHERE org_id = $1), 0), count(*) FILTER (WHERE org_id IS NULL)
		 FROM "livekit_analytics_test"."room_byte_samples"`,
		"org_01HQZX",
	).Scan(&orgBytes, &unattributedRows))
	require.EqualValues(t, 361, orgBytes)
	require.EqualValues(t, 1, unattributedRows)

	// packet counts round-trip alongside the bytes they explain, and packets sums
	// them the same way bytes sums the byte columns - the point being that a rollup
	// converting recorded bytes to actual wire cost never has to remember to add
	// all three components itself
	var primaryPackets, retransmitPackets, totalPackets int64
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT primary_packets, retransmit_packets, packets FROM "livekit_analytics_test"."room_byte_samples" WHERE participant_id = $1`,
		"PA_1",
	).Scan(&primaryPackets, &retransmitPackets, &totalPackets))
	require.EqualValues(t, 9, primaryPackets)
	require.EqualValues(t, 2, retransmitPackets)
	require.EqualValues(t, 11, totalPackets)

	// A row written before the packet columns existed has NULL in all three, the
	// same way a fresh ADD COLUMN leaves every pre-existing row NULL. packets must
	// stay NULL for it too, not fall back to 0 - a rollup needs to tell "known zero
	// packets" apart from "packet counts unavailable for this row", and a silent 0
	// would understate that room's overhead conversion instead of skipping it.
	// store.insert always supplies real int64 packet counts (roomByteSample has no
	// nullable wrapper for them, on the assumption that the sink always has real
	// counts to write - see PrimaryPackets' field comment), so this simulates the
	// legacy case directly in SQL rather than through the Go struct.
	_, err = store.pool.Exec(ctx,
		`INSERT INTO "livekit_analytics_test"."room_byte_samples"
		 (room_name, room_id, participant_id, track_id, direction, primary_bytes, retransmit_bytes, padding_bytes, sampled_at, node_id)
		 VALUES ('world-org-1', 'RM_a', 'PA_legacy', 'TR_legacy', 'upstream', 1, 0, 0, now(), 'ND_1')`,
	)
	require.NoError(t, err)

	var packetsNull bool
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT packets IS NULL FROM "livekit_analytics_test"."room_byte_samples" WHERE participant_id = $1`,
		"PA_legacy",
	).Scan(&packetsNull))
	require.True(t, packetsNull)

	// participant_kind lets a rollup exclude non-billable participants by name
	var egressRows int64
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT count(*) FROM "livekit_analytics_test"."room_byte_samples" WHERE participant_kind = 'EGRESS'`,
	).Scan(&egressRows))
	require.EqualValues(t, 1, egressRows)
}

// A retried COPY must not be able to duplicate a sample: this is the entire reason
// uniqueSampleConstraint exists. Two batches that describe the same logical sample
// - same node, room, participant, track, direction and interval - must collide
// instead of both landing.
func TestStoreRejectsADuplicateSample(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(analyticsTestDSNEnv))
	if dsn == "" {
		t.Skipf("set %s to run the analytics store integration test", analyticsTestDSNEnv)
	}

	conf, err := config.PostgresAnalyticsConfig{DSN: dsn, Schema: "livekit_analytics_dup_test"}.Resolved()
	require.NoError(t, err)

	store, err := newPgAnalyticsStore(conf)
	require.NoError(t, err)
	t.Cleanup(store.close)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, err := store.pool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "livekit_analytics_dup_test" CASCADE`)
		require.NoError(t, err)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, store.migrate(ctx))

	sample := roomByteSample{
		RoomName: "world-org-1", RoomID: "RM_a", ParticipantID: "PA_1", TrackID: "TR_1",
		Direction: directionUpstream, PrimaryBytes: 100,
		SampledAt: time.Now().UTC().Truncate(time.Millisecond), NodeID: "ND_1",
	}

	require.NoError(t, store.insert(ctx, []roomByteSample{sample}))

	// same (node_id, room_id, participant_id, track_id, direction, sampled_at) as
	// above - as a retried batch for the same interval would be - must be rejected,
	// not silently written a second time
	err = store.insert(ctx, []roomByteSample{sample})
	require.Error(t, err)

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23505", pgErr.Code, "expected a unique_violation from uniqueSampleConstraint")

	var rows int64
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT count(*) FROM "livekit_analytics_dup_test"."room_byte_samples"`,
	).Scan(&rows))
	require.EqualValues(t, 1, rows, "the rejected batch must not have written anything")
}
