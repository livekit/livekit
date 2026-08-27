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

	// applicationName identifies the sink's connections in pg_stat_activity.
	analyticsApplicationName = "livekit-analytics-sink"
)

// roomByteSampleColumns must stay in the order used by roomByteSample.copyRow.
var roomByteSampleColumns = []string{
	"room_name",
	"room_id",
	"participant_id",
	"track_id",
	"direction",
	"primary_bytes",
	"retransmit_bytes",
	"padding_bytes",
	"sampled_at",
	"node_id",
}

// concurrentDDLCodes are the SQLSTATEs a losing racer sees when several nodes run
// the idempotent migration at the same time. Postgres' IF NOT EXISTS is not
// race-free, so they are treated as success rather than as failures.
var concurrentDDLCodes = map[string]struct{}{
	"23505": {}, // unique_violation on a catalog index
	"42P06": {}, // duplicate_schema
	"42P07": {}, // duplicate_table
	"42710": {}, // duplicate_object
}

// roomByteSample is one (room, participant, track, direction) byte count as
// reported by the SFU for a single stats interval. The counters are per-interval
// deltas, so summing rows over a period yields the bytes moved in that period.
type roomByteSample struct {
	RoomName        string
	RoomID          string
	ParticipantID   string
	TrackID         string
	Direction       string
	PrimaryBytes    int64
	RetransmitBytes int64
	PaddingBytes    int64
	SampledAt       time.Time
	NodeID          string
}

func (s roomByteSample) copyRow() []any {
	return []any{
		s.RoomName,
		s.RoomID,
		s.ParticipantID,
		s.TrackID,
		s.Direction,
		s.PrimaryBytes,
		s.RetransmitBytes,
		s.PaddingBytes,
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

// migrate creates the schema, table and indexes when they are missing. It is
// idempotent and safe to run concurrently from every node.
func (s *pgAnalyticsStore) migrate(ctx context.Context) error {
	for _, stmt := range s.migrationStatements() {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			if isConcurrentDDLError(err) {
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
func (s *pgAnalyticsStore) migrationStatements() []string {
	schema := pgx.Identifier{s.schema}.Sanitize()
	table := s.target.Sanitize()

	return []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	room_name        text        NOT NULL,
	room_id          text        NOT NULL,
	participant_id   text        NOT NULL,
	track_id         text        NOT NULL,
	direction        text        NOT NULL CHECK (direction IN ('upstream', 'downstream')),
	primary_bytes    bigint      NOT NULL,
	retransmit_bytes bigint      NOT NULL,
	padding_bytes    bigint      NOT NULL,
	bytes            bigint      NOT NULL GENERATED ALWAYS AS (primary_bytes + retransmit_bytes + padding_bytes) STORED,
	sampled_at       timestamptz NOT NULL,
	inserted_at      timestamptz NOT NULL DEFAULT now(),
	node_id          text        NOT NULL
)`, table),
		fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON %s (room_name, sampled_at)`,
			pgx.Identifier{roomByteSamplesTable + "_room_sampled_at_idx"}.Sanitize(), table,
		),
		fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON %s (inserted_at)`,
			pgx.Identifier{roomByteSamplesTable + "_inserted_at_idx"}.Sanitize(), table,
		),
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

func isConcurrentDDLError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	_, ok := concurrentDDLCodes[pgErr.Code]
	return ok
}
