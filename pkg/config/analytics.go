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

	FlushInterval  time.Duration `yaml:"flush_interval,omitempty"`
	BatchSize      int           `yaml:"batch_size,omitempty"`
	BufferSize     int           `yaml:"buffer_size,omitempty"`
	MaxConns       int32         `yaml:"max_conns,omitempty"`
	ConnectTimeout time.Duration `yaml:"connect_timeout,omitempty"`
	WriteTimeout   time.Duration `yaml:"write_timeout,omitempty"`
}

var DefaultAnalyticsConfig = AnalyticsConfig{
	Postgres: PostgresAnalyticsConfig{
		Schema:         DefaultAnalyticsPostgresSchema,
		AutoMigrate:    true,
		FlushInterval:  DefaultAnalyticsPostgresFlushInterval,
		BatchSize:      DefaultAnalyticsPostgresBatchSize,
		BufferSize:     DefaultAnalyticsPostgresBufferSize,
		MaxConns:       DefaultAnalyticsPostgresMaxConns,
		ConnectTimeout: DefaultAnalyticsPostgresConnectTimeout,
		WriteTimeout:   DefaultAnalyticsPostgresWriteTimeout,
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
