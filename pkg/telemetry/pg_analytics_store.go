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

// Fork addition: Postgres storage for the analytics sink. See
// pg_analytics_service.go for the AnalyticsService implementation that feeds it.

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/livekit/livekit-server/pkg/config"
)

const (
	roomByteSamplesTable = "room_byte_samples"

	// uniqueSampleConstraint makes a retried COPY idempotent: two batches that
	// write the same logical sample collide on this instead of duplicating the
	// row. See docs/analytics-sink.md, "A retried COPY can duplicate rows".
	uniqueSampleConstraint = "room_byte_samples_unique_sample"

	// applicationName identifies the sink's connections in pg_stat_activity.
	analyticsApplicationName = "livekit-analytics-sink"
)

// roomByteSampleColumns must stay in the order used by roomByteSample.copyRow.
var roomByteSampleColumns = []string{
	"org_id",
	"room_name",
	"room_id",
	"participant_id",
	"participant_kind",
	"track_id",
	"direction",
	"primary_bytes",
	"retransmit_bytes",
	"padding_bytes",
	"primary_packets",
	"retransmit_packets",
	"padding_packets",
	"sampled_at",
	"node_id",
}

// concurrentDDLCodes are the SQLSTATEs a losing racer sees when several nodes run
// the idempotent migration at the same time. Postgres' IF NOT EXISTS is not
// race-free, so they are treated as success rather than as failures.
//
// This is the default set, used by every step except the one adding
// uniqueSampleConstraint - see constraintRaceCodes for why that one is narrower.
var concurrentDDLCodes = map[string]struct{}{
	"23505": {}, // unique_violation on a catalog index
	"42P06": {}, // duplicate_schema
	"42P07": {}, // duplicate_table
	"42701": {}, // duplicate_column
	"42710": {}, // duplicate_object
}

// constraintRaceCodes governs adding uniqueSampleConstraint. Postgres has no
// `ADD CONSTRAINT IF NOT EXISTS`; adding a UNIQUE constraint creates a backing
// index under the constraint's name, so a second node (or a second migrate() call
// on the same node) adding the same named constraint sees 42P07 (duplicate_table -
// Postgres' generic "relation already exists", which covers index names too, not
// just tables) - safe to treat as "already there". Confirmed against a real server
// rather than assumed: see TestStoreWritesSamples, which calls migrate() twice.
//
// 23505 (unique_violation) is deliberately excluded even though it is in
// concurrentDDLCodes: for this specific statement it does not mean a race, it
// means the table already holds rows that violate the constraint being added -
// real duplicate samples from the COPY-retry gap this constraint exists to close.
// Swallowing that would silently leave the table unprotected while claiming it
// is fixed, so it is left to fail the migration and block startup instead, the
// same fail-fast treatment as an unreachable database.
var constraintRaceCodes = map[string]struct{}{
	"42P07": {}, // duplicate_table (also raised for a duplicate index/constraint name)
}

// roomByteSample is one (room, participant, track, direction) byte count as
// reported by the SFU for a single stats interval. The counters are per-interval
// deltas, so summing rows over a period yields the bytes moved in that period.
type roomByteSample struct {
	// OrgID is the organization the bytes are billed to, resolved from the
	// participant's token attribute. Empty when it could not be resolved, which is
	// written as NULL rather than dropping the row: unattributed usage is a problem
	// to investigate, missing usage is one that cannot be.
	OrgID         string
	RoomName      string
	RoomID        string
	ParticipantID string

	// ParticipantKind is the participant's role (STANDARD, EGRESS, INGRESS, AGENT,
	// ...), stored as text so a rollup can exclude non-billable participants by
	// name without a lookup table. Empty when the participant was never resolved -
	// written as NULL, the same rule as OrgID.
	ParticipantKind string

	TrackID   string
	Direction string

	PrimaryBytes    int64
	RetransmitBytes int64
	PaddingBytes    int64

	// PrimaryPackets, RetransmitPackets and PaddingPackets are the packet counts
	// behind the byte counts above, from the same AnalyticsStream. A recorded byte
	// cannot be converted into the bandwidth actually billed by a cloud provider -
	// which additionally counts the IP/UDP/SRTP overhead added to every packet -
	// without knowing how many packets that byte count was split across; the ratio
	// swings from about 1.04x to 1.41x depending on the room's media mix. See
	// docs/analytics-sink.md.
	PrimaryPackets    int64
	RetransmitPackets int64
	PaddingPackets    int64

	SampledAt time.Time
	NodeID    string
}

func (s roomByteSample) copyRow() []any {
	return []any{
		nullableText(s.OrgID),
		s.RoomName,
		s.RoomID,
		s.ParticipantID,
		nullableText(s.ParticipantKind),
		s.TrackID,
		s.Direction,
		s.PrimaryBytes,
		s.RetransmitBytes,
		s.PaddingBytes,
		s.PrimaryPackets,
		s.RetransmitPackets,
		s.PaddingPackets,
		s.SampledAt,
		s.NodeID,
	}
}

// pgAnalyticsStore owns the connection pool and the schema the samples land in.
// Deadlines belong to the caller: every method honours the context it is given.
type pgAnalyticsStore struct {
	pool   *pgxpool.Pool
	schema string

	// target is the fully qualified, sanitized table identifier used by COPY.
	target pgx.Identifier
}

// newPgAnalyticsStore validates the DSN and builds the pool. No connection is
// opened here: pgx connects lazily, so a database that is temporarily down does not
// prevent the SFU from starting.
func newPgAnalyticsStore(conf config.PostgresAnalyticsConfig) (*pgAnalyticsStore, error) {
	poolConf, err := pgxpool.ParseConfig(conf.DSN)
	if err != nil {
		// pgx redacts the password in this error, but not the rest of the DSN, so it
		// is not forwarded to logs by callers - only to the startup failure.
		return nil, fmt.Errorf("invalid analytics postgres dsn: %w", err)
	}

	poolConf.MaxConns = conf.MaxConns
	poolConf.ConnConfig.ConnectTimeout = conf.ConnectTimeout
	if poolConf.ConnConfig.RuntimeParams == nil {
		poolConf.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolConf.ConnConfig.RuntimeParams["application_name"] = analyticsApplicationName

	pool, err := pgxpool.NewWithConfig(context.Background(), poolConf)
	if err != nil {
		return nil, fmt.Errorf("could not create analytics postgres pool: %w", err)
	}

	return &pgAnalyticsStore{
		pool:   pool,
		schema: conf.Schema,
		target: pgx.Identifier{conf.Schema, roomByteSamplesTable},
	}, nil
}

// migrationStep is one DDL statement plus the SQLSTATEs that mean "another node
// already did this concurrently" for that specific statement. Almost every step
// shares the same default set (concurrentDDLCodes); the constraint step below is
// the one exception.
type migrationStep struct {
	stmt      string
	raceCodes map[string]struct{}
}

func step(stmt string) migrationStep {
	return migrationStep{stmt: stmt, raceCodes: concurrentDDLCodes}
}

// migrate creates the schema, table and indexes when they are missing. It is
// idempotent and safe to run concurrently from every node - except for the
// uniqueSampleConstraint step, which is idempotent only when the table does not
// already contain the duplicates it exists to prevent; see constraintRaceCodes.
func (s *pgAnalyticsStore) migrate(ctx context.Context) error {
	for _, step := range s.migrationStatements() {
		if _, err := s.pool.Exec(ctx, step.stmt); err != nil {
			if isRaceError(err, step.raceCodes) {
				continue
			}
			return err
		}
	}
	return nil
}

// migrationStatements returns the DDL for the sink's own schema. The schema name is
// validated by config.PostgresAnalyticsConfig.Resolved and sanitized again here; no
// other part of the statements is caller-controlled.
func (s *pgAnalyticsStore) migrationStatements() []migrationStep {
	schema := pgx.Identifier{s.schema}.Sanitize()
	table := s.target.Sanitize()
	constraint := pgx.Identifier{uniqueSampleConstraint}.Sanitize()

	return []migrationStep{
		step(fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schema)),
		step(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	org_id              text,
	room_name           text        NOT NULL,
	room_id             text        NOT NULL,
	participant_id      text        NOT NULL,
	participant_kind    text,
	track_id            text        NOT NULL,
	direction           text        NOT NULL CHECK (direction IN ('upstream', 'downstream')),
	primary_bytes       bigint      NOT NULL,
	retransmit_bytes    bigint      NOT NULL,
	padding_bytes       bigint      NOT NULL,
	bytes               bigint      NOT NULL GENERATED ALWAYS AS (primary_bytes + retransmit_bytes + padding_bytes) STORED,
	primary_packets     bigint,
	retransmit_packets  bigint,
	padding_packets     bigint,
	packets             bigint      GENERATED ALWAYS AS (primary_packets + retransmit_packets + padding_packets) STORED,
	sampled_at          timestamptz NOT NULL,
	inserted_at         timestamptz NOT NULL DEFAULT now(),
	node_id             text        NOT NULL
)`, table)),
		// No server has run this schema yet, so every column so far lives directly in
		// CREATE TABLE rather than behind an ADD COLUMN IF NOT EXISTS upgrade step -
		// there is no deployed table to upgrade in place. The first column added
		// after a real deployment exists is what should introduce that pattern back
		// (nullable, no default, so it does not rewrite the table at the ~1M
		// rows/day this table reaches - see docs/analytics-sink.md), not before.
		//
		// makes a retried COPY idempotent - see uniqueSampleConstraint and
		// constraintRaceCodes for what happens if the table already has duplicates.
		{
			stmt: fmt.Sprintf(
				`ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (node_id, room_id, participant_id, track_id, direction, sampled_at)`,
				table, constraint,
			),
			raceCodes: constraintRaceCodes,
		},
		step(fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON %s (org_id, sampled_at)`,
			pgx.Identifier{roomByteSamplesTable + "_org_sampled_at_idx"}.Sanitize(), table,
		)),
		step(fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON %s (room_name, sampled_at)`,
			pgx.Identifier{roomByteSamplesTable + "_room_sampled_at_idx"}.Sanitize(), table,
		)),
		step(fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON %s (inserted_at)`,
			pgx.Identifier{roomByteSamplesTable + "_inserted_at_idx"}.Sanitize(), table,
		)),
	}
}

// insert writes a batch with COPY. COPY runs in a single implicit transaction, so a
// failed batch inserts nothing and can be retried without creating duplicates.
func (s *pgAnalyticsStore) insert(ctx context.Context, samples []roomByteSample) error {
	if len(samples) == 0 {
		return nil
	}

	_, err := s.pool.CopyFrom(ctx, s.target, roomByteSampleColumns, pgx.CopyFromSlice(
		len(samples),
		func(i int) ([]any, error) { return samples[i].copyRow(), nil },
	))
	return err
}

func (s *pgAnalyticsStore) close() {
	s.pool.Close()
}

// logFields describes the connection without exposing the password.
func (s *pgAnalyticsStore) logFields() []any {
	conn := s.pool.Config().ConnConfig
	return []any{
		"host", conn.Host,
		"port", conn.Port,
		"database", conn.Database,
		"user", conn.User,
		"schema", s.schema,
	}
}

// nullableText writes an unset value as SQL NULL, keeping "no organization could
// be resolved" distinguishable from an organization whose id is an empty string.
func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func isRaceError(err error, codes map[string]struct{}) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	_, ok := codes[pgErr.Code]
	return ok
}
