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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestAnalyticsDefaultsAreLoaded(t *testing.T) {
	conf, err := NewConfig("", true, nil, nil)
	require.NoError(t, err)

	require.False(t, conf.Analytics.Postgres.IsConfigured())
	require.Equal(t, DefaultAnalyticsPostgresSchema, conf.Analytics.Postgres.Schema)
	require.True(t, conf.Analytics.Postgres.AutoMigrate)
	require.Equal(t, DefaultAnalyticsPostgresFlushInterval, conf.Analytics.Postgres.FlushInterval)
}

func TestAnalyticsConfigIsReadFromYAML(t *testing.T) {
	conf, err := NewConfig(`
analytics:
  postgres:
    dsn: postgres://livekit:secret@db:5432/hideout
    schema: billing_samples
    auto_migrate: false
    org_attribute_key: tenantId
    flush_interval: 2s
`, true, nil, nil)
	require.NoError(t, err)

	pg := conf.Analytics.Postgres
	require.True(t, pg.IsConfigured())
	require.Equal(t, "billing_samples", pg.Schema)
	require.False(t, pg.AutoMigrate)
	require.Equal(t, "tenantId", pg.OrgAttributeKey)
	require.Equal(t, 2*time.Second, pg.FlushInterval)
}

func TestPostgresAnalyticsConfigResolvedFillsDefaults(t *testing.T) {
	resolved, err := PostgresAnalyticsConfig{DSN: " postgres://db/hideout "}.Resolved()
	require.NoError(t, err)

	require.Equal(t, "postgres://db/hideout", resolved.DSN)
	require.Equal(t, DefaultAnalyticsPostgresSchema, resolved.Schema)
	require.Equal(t, DefaultAnalyticsOrgAttributeKey, resolved.OrgAttributeKey)
	require.Equal(t, DefaultAnalyticsPostgresBatchSize, resolved.BatchSize)
	require.Equal(t, DefaultAnalyticsPostgresBufferSize, resolved.BufferSize)
	require.EqualValues(t, DefaultAnalyticsPostgresMaxConns, resolved.MaxConns)
	require.Equal(t, DefaultAnalyticsPostgresWriteTimeout, resolved.WriteTimeout)
}

// The prefix is defaulted where the config is loaded, not in Resolved, so that an
// operator can switch the room-name cross-check off with an explicit empty value.
func TestPostgresAnalyticsConfigResolvedLeavesTheRoomNamePrefixAlone(t *testing.T) {
	loaded, err := NewConfig(`
analytics:
  postgres:
    dsn: postgres://db/hideout
`, true, nil, nil)
	require.NoError(t, err)
	resolved, err := loaded.Analytics.Postgres.Resolved()
	require.NoError(t, err)
	require.Equal(t, DefaultAnalyticsOrgRoomNamePrefix, resolved.OrgRoomNamePrefix)

	disabled, err := NewConfig(`
analytics:
  postgres:
    dsn: postgres://db/hideout
    org_room_name_prefix: ""
`, true, nil, nil)
	require.NoError(t, err)
	resolved, err = disabled.Analytics.Postgres.Resolved()
	require.NoError(t, err)
	require.Empty(t, resolved.OrgRoomNamePrefix, "an explicit empty value must survive as \"check disabled\"")
}

// The attribute name is what apps/api writes onto the token it mints. A configured
// value must survive Resolved untouched, or every row silently loses its
// organization the moment the two sides agree on a name other than the default.
func TestPostgresAnalyticsConfigResolvedKeepsConfiguredOrgAttributeKey(t *testing.T) {
	resolved, err := PostgresAnalyticsConfig{
		DSN:             "postgres://db/hideout",
		OrgAttributeKey: "  tenantId  ",
	}.Resolved()
	require.NoError(t, err)
	require.Equal(t, "tenantId", resolved.OrgAttributeKey)
}

func TestPostgresAnalyticsConfigResolvedKeepsBufferAtLeastOneBatch(t *testing.T) {
	resolved, err := PostgresAnalyticsConfig{DSN: "postgres://db/hideout", BatchSize: 500, BufferSize: 10}.Resolved()
	require.NoError(t, err)
	require.Equal(t, 500, resolved.BufferSize)
}

func TestPostgresAnalyticsConfigResolvedRejectsUnsafeSchema(t *testing.T) {
	for _, schema := range []string{
		`public"; DROP TABLE invoices; --`,
		"Mixed_Case",
		"1_leading_digit",
		"has space",
		strings.Repeat("a", maxPostgresIdentifierLength+1),
	} {
		_, err := PostgresAnalyticsConfig{DSN: "postgres://db/hideout", Schema: schema}.Resolved()
		require.ErrorIs(t, err, ErrAnalyticsSchemaInvalid, "schema %q must be rejected", schema)
	}
}

func TestPostgresAnalyticsConfigResolvedReadsDSNFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.dsn")
	require.NoError(t, os.WriteFile(path, []byte("postgres://livekit:secret@db:5432/hideout\n"), 0o600))

	resolved, err := PostgresAnalyticsConfig{DSNFile: path}.Resolved()
	require.NoError(t, err)
	require.Equal(t, "postgres://livekit:secret@db:5432/hideout", resolved.DSN)
}

func TestPostgresAnalyticsConfigResolvedRejectsWorldReadableDSNFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.dsn")
	require.NoError(t, os.WriteFile(path, []byte("postgres://db/hideout"), 0o644))

	_, err := PostgresAnalyticsConfig{DSNFile: path}.Resolved()
	require.ErrorIs(t, err, ErrAnalyticsDSNFileIncorrectPermission)
}

func TestPostgresAnalyticsConfigResolvedRejectsEmptyDSNFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.dsn")
	require.NoError(t, os.WriteFile(path, []byte("   \n"), 0o600))

	_, err := PostgresAnalyticsConfig{DSNFile: path}.Resolved()
	require.ErrorIs(t, err, ErrAnalyticsDSNFileEmpty)
}

func TestPostgresAnalyticsConfigResolvedRejectsBothDSNAndFile(t *testing.T) {
	_, err := PostgresAnalyticsConfig{DSN: "postgres://db/hideout", DSNFile: "/tmp/analytics.dsn"}.Resolved()
	require.ErrorIs(t, err, ErrAnalyticsDSNAmbiguous)
}

func TestAnalyticsPostgresDSNIsSettableFromCLIAndEnv(t *testing.T) {
	flags, err := GenerateCLIFlags(nil, false)
	require.NoError(t, err)

	c := &cli.Command{Name: "test"}
	c.Flags = append(c.Flags, flags...)
	require.NoError(t, c.Set("analytics.postgres.dsn", "postgres://db/hideout"))

	conf, err := NewConfig("", true, c, nil)
	require.NoError(t, err)
	require.Equal(t, "postgres://db/hideout", conf.Analytics.Postgres.DSN)

	var dsnFlag cli.Flag
	for _, flag := range flags {
		if slices.Contains(flag.Names(), "analytics.postgres.dsn") {
			dsnFlag = flag
		}
	}
	require.NotNil(t, dsnFlag)
	// the sink's DSN is expected to be supplied as an environment variable in production
	require.Contains(t, fmt.Sprint(dsnFlag.(*cli.StringFlag).Sources), "LIVEKIT_ANALYTICS_POSTGRES_DSN")
}
