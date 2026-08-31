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

// Fork addition: configuration for the self-hosted analytics sink, which records
// per-room media byte counters to Postgres so that usage can be invoiced from
// server-side ground truth instead of client-reported numbers.
//
// It lives in its own file so that the diff against upstream config.go stays at a
// single struct field, keeping livekit-server upgrades cheap to rebase.

package config

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultAnalyticsPostgresSchema is the Postgres schema owned by the sink. The
	// sink shares the application's database and is isolated by schema, not by
	// database, so nothing outside this schema is ever touched.
	DefaultAnalyticsPostgresSchema = "livekit_analytics"

	// DefaultAnalyticsPostgresFlushInterval is how often buffered samples are written.
	// Upstream telemetry hands stats to the sink every 30s, so this only bounds how
	// long a sample sits in memory after a batch is cut short.
	DefaultAnalyticsPostgresFlushInterval = 5 * time.Second

	// DefaultAnalyticsPostgresBatchSize is the maximum number of rows per COPY.
	DefaultAnalyticsPostgresBatchSize = 1000

	// DefaultAnalyticsPostgresBufferSize bounds how many samples are held in memory
	// while Postgres is unreachable. At ~400 samples per 30s for a busy room this
	// covers roughly an hour of downtime before samples start being dropped.
	DefaultAnalyticsPostgresBufferSize = 50000

	// DefaultAnalyticsPostgresMaxConns bounds the pool; a single writer goroutine
	// issues the COPYs, the spare connections cover migration and reconnects.
	DefaultAnalyticsPostgresMaxConns = 4

	DefaultAnalyticsPostgresConnectTimeout = 10 * time.Second
	DefaultAnalyticsPostgresWriteTimeout   = 30 * time.Second

	// DefaultAnalyticsOrgAttributeKey is the participant attribute the sink reads
	// the billable organization id from. apps/api sets it on the LiveKit access
	// token it mints, so the value is signed with the LiveKit API secret and is
	// server-issued rather than client-supplied.
	//
	// This is a JWT attribute name, not the column it lands in: the attribute is
	// camelCase because apps/api names it that way, the column is always org_id.
	DefaultAnalyticsOrgAttributeKey = "orgId"

	// DefaultAnalyticsOrgRoomNamePrefix is the room-name prefix whose remainder is
	// the organization id, so that the id recorded from the token can be checked
	// against the room the bytes actually moved in. Rooms whose name does not start
	// with it - private desk rooms, named after a zone - are simply not checked.
	//
	// Set org_room_name_prefix to an empty string to turn the check off. Unlike the
	// other defaults this one is applied when the config is loaded rather than by
	// Resolved, so that an explicit empty value survives.
	DefaultAnalyticsOrgRoomNamePrefix = "world-"

	// maxPostgresIdentifierLength is Postgres' NAMEDATALEN - 1.
	maxPostgresIdentifierLength = 63
)

var (
	ErrAnalyticsSchemaInvalid              = errors.New("analytics.postgres.schema must match [a-z_][a-z0-9_]* and be at most 63 characters")
	ErrAnalyticsDSNAmbiguous               = errors.New("analytics.postgres: set only one of dsn or dsn_file")
	ErrAnalyticsDSNFileEmpty               = errors.New("analytics.postgres.dsn_file is empty")
	ErrAnalyticsDSNFileIncorrectPermission = errors.New("analytics.postgres.dsn_file others permissions must be set to 0")
)

// analyticsSchemaPattern deliberately allows only lowercase unquoted identifiers.
// The schema name is interpolated into DDL, so it is validated here rather than
// trusted, even though it also goes through pgx identifier sanitization.
var analyticsSchemaPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

type AnalyticsConfig struct {
	Postgres PostgresAnalyticsConfig `yaml:"postgres,omitempty"`
}

// PostgresAnalyticsConfig configures the Postgres analytics sink. When no DSN is
// configured the server keeps upstream's analytics behaviour (no sink) and no
// billing samples are recorded.
type PostgresAnalyticsConfig struct {
	// DSN is the libpq/URL connection string for the application database, e.g.
	// postgres://user:pass@host:5432/hideout. Prefer DSNFile or the
	// LIVEKIT_ANALYTICS_POSTGRES_DSN environment variable over committing it to yaml.
	DSN string `yaml:"dsn,omitempty"`

	// DSNFile reads the DSN from a file, mirroring how TURN secrets are handled.
	// The file must not be readable by others.
	DSNFile string `yaml:"dsn_file,omitempty"`

	// Schema is the Postgres schema the sink creates and writes to.
	Schema string `yaml:"schema,omitempty"`

	// AutoMigrate creates the schema, table and indexes at startup when missing.
	AutoMigrate bool `yaml:"auto_migrate,omitempty"`

	// OrgAttributeKey is the participant attribute holding the organization a
	// participant's traffic is billed to. It is configurable because apps/api and
	// this server deploy independently: renaming the attribute on one side would
	// otherwise need a coordinated rebuild of the other.
	OrgAttributeKey string `yaml:"org_attribute_key,omitempty"`

	// OrgRoomNamePrefix enables the consistency check between the organization on
	// the participant's token and the one in the room name. Empty disables it.
	//
	// The check never changes what is recorded - the token stays authoritative - it
	// only counts and logs disagreements, because a silent disagreement between the
	// two sources of the same id is exactly what makes a wrong invoice undetectable.
	OrgRoomNamePrefix string `yaml:"org_room_name_prefix,omitempty"`

	FlushInterval  time.Duration `yaml:"flush_interval,omitempty"`
	BatchSize      int           `yaml:"batch_size,omitempty"`
	BufferSize     int           `yaml:"buffer_size,omitempty"`
	MaxConns       int32         `yaml:"max_conns,omitempty"`
	ConnectTimeout time.Duration `yaml:"connect_timeout,omitempty"`
	WriteTimeout   time.Duration `yaml:"write_timeout,omitempty"`
}

var DefaultAnalyticsConfig = AnalyticsConfig{
	Postgres: PostgresAnalyticsConfig{
		Schema:            DefaultAnalyticsPostgresSchema,
		AutoMigrate:       true,
		OrgAttributeKey:   DefaultAnalyticsOrgAttributeKey,
		OrgRoomNamePrefix: DefaultAnalyticsOrgRoomNamePrefix,
		FlushInterval:     DefaultAnalyticsPostgresFlushInterval,
		BatchSize:         DefaultAnalyticsPostgresBatchSize,
		BufferSize:        DefaultAnalyticsPostgresBufferSize,
		MaxConns:          DefaultAnalyticsPostgresMaxConns,
		ConnectTimeout:    DefaultAnalyticsPostgresConnectTimeout,
		WriteTimeout:      DefaultAnalyticsPostgresWriteTimeout,
	},
}

// IsConfigured reports whether a DSN was supplied, either inline or by file.
func (c PostgresAnalyticsConfig) IsConfigured() bool {
	return strings.TrimSpace(c.DSN) != "" || strings.TrimSpace(c.DSNFile) != ""
}

// Resolved returns a copy with the DSN loaded from DSNFile (when used), defaults
// filled in for unset values, and the schema name validated. The returned value is
// what the sink should run with; the receiver is left untouched so that the loaded
// DSN is never written back into the shared config struct.
func (c PostgresAnalyticsConfig) Resolved() (PostgresAnalyticsConfig, error) {
	resolved := c
	resolved.DSN = strings.TrimSpace(c.DSN)
	resolved.DSNFile = strings.TrimSpace(c.DSNFile)

	if resolved.DSNFile != "" {
		if resolved.DSN != "" {
			return resolved, ErrAnalyticsDSNAmbiguous
		}
		dsn, err := readSecretFile(resolved.DSNFile)
		if err != nil {
			return resolved, err
		}
		if dsn == "" {
			return resolved, ErrAnalyticsDSNFileEmpty
		}
		resolved.DSN = dsn
	}

	if resolved.Schema == "" {
		resolved.Schema = DefaultAnalyticsPostgresSchema
	}
	resolved.OrgAttributeKey = strings.TrimSpace(resolved.OrgAttributeKey)
	if resolved.OrgAttributeKey == "" {
		resolved.OrgAttributeKey = DefaultAnalyticsOrgAttributeKey
	}
	// deliberately not defaulted here: DefaultAnalyticsConfig supplies it when the
	// key is absent, so an explicit empty value reaches the sink as "check disabled"
	resolved.OrgRoomNamePrefix = strings.TrimSpace(resolved.OrgRoomNamePrefix)
	if !isValidPostgresIdentifier(resolved.Schema) {
		return resolved, ErrAnalyticsSchemaInvalid
	}

	if resolved.FlushInterval <= 0 {
		resolved.FlushInterval = DefaultAnalyticsPostgresFlushInterval
	}
	if resolved.BatchSize <= 0 {
		resolved.BatchSize = DefaultAnalyticsPostgresBatchSize
	}
	if resolved.BufferSize <= 0 {
		resolved.BufferSize = DefaultAnalyticsPostgresBufferSize
	}
	if resolved.BufferSize < resolved.BatchSize {
		resolved.BufferSize = resolved.BatchSize
	}
	if resolved.MaxConns <= 0 {
		resolved.MaxConns = DefaultAnalyticsPostgresMaxConns
	}
	if resolved.ConnectTimeout <= 0 {
		resolved.ConnectTimeout = DefaultAnalyticsPostgresConnectTimeout
	}
	if resolved.WriteTimeout <= 0 {
		resolved.WriteTimeout = DefaultAnalyticsPostgresWriteTimeout
	}

	return resolved, nil
}

func isValidPostgresIdentifier(name string) bool {
	return len(name) <= maxPostgresIdentifierLength && analyticsSchemaPattern.MatchString(name)
}

// readSecretFile reads a credential file, refusing world-readable ones the same way
// key and TURN secret files are handled.
func readSecretFile(path string) (string, error) {
	const otherFilter os.FileMode = 0o007

	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&otherFilter != 0o000 {
		return "", ErrAnalyticsDSNFileIncorrectPermission
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
