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

// Fork addition: a disposable Postgres for the end to end analytics tests.
//
// It is started the same way pkg/service starts redis, so an ordinary
// `go test ./test/` exercises the sink against a real database instead of skipping
// it. Set LIVEKIT_TEST_ANALYTICS_POSTGRES_DSN to point at a database that is already
// running, or pass -analytics.docker=false to skip the analytics tests entirely.

package test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

const (
	// analyticsTestDSNEnv points the analytics tests at a Postgres that is already
	// running. When it is empty they start one in docker.
	analyticsTestDSNEnv = "LIVEKIT_TEST_ANALYTICS_POSTGRES_DSN"

	// analyticsPostgresTag is pinned: these tests assert on COPY and on concurrent
	// DDL behaviour, so a silently newer server is a silently different test.
	analyticsPostgresTag = "17.6-alpine"
)

// -analytics.docker=false skips the analytics tests that need a docker daemon, for a
// checkout without one. Running them is the default: billing telemetry that is only
// covered when someone remembers to export a DSN is not covered.
var useAnalyticsDocker = flag.Bool("analytics.docker", true, "run the analytics tests that need a docker daemon")

var (
	analyticsPoolOnce sync.Once
	analyticsPool     dockertest.ClosablePool
	analyticsPoolErr  error
	analyticsPGLast   atomic.Uint32
)

func analyticsDockerPool(t testing.TB) dockertest.ClosablePool {
	t.Helper()

	analyticsPoolOnce.Do(func() {
		ctx := context.Background()

		pool, err := dockertest.NewPool(ctx, "")
		if err != nil {
			analyticsPoolErr = fmt.Errorf("could not construct docker pool: %w", err)
			return
		}
		if _, err := pool.Client().Ping(ctx, mobyclient.PingOptions{}); err != nil {
			analyticsPoolErr = fmt.Errorf("could not connect to docker: %w", err)
			return
		}
		analyticsPool = pool
	})

	require.NoError(t, analyticsPoolErr,
		"analytics tests need a docker daemon; pass -analytics.docker=false or set %s", analyticsTestDSNEnv)
	return analyticsPool
}

// analyticsPostgres returns a DSN for a throwaway Postgres, started in docker unless
// one was supplied through the environment. The container is removed with the test.
func analyticsPostgres(t testing.TB) string {
	t.Helper()

	if dsn := strings.TrimSpace(os.Getenv(analyticsTestDSNEnv)); dsn != "" {
		return dsn
	}
	if !*useAnalyticsDocker {
		t.Skipf("this test needs a docker daemon or %s, and -analytics.docker=false says there is none", analyticsTestDSNEnv)
	}

	pool := analyticsDockerPool(t)
	container, err := pool.Run(t.Context(), "postgres",
		dockertest.WithTag(analyticsPostgresTag),
		dockertest.WithName(fmt.Sprintf("lktest-analytics-pg-%d", analyticsPGLast.Inc())),
		dockertest.WithEnv([]string{
			"POSTGRES_USER=postgres",
			"POSTGRES_PASSWORD=postgres",
			"POSTGRES_DB=postgres",
		}),
		// nothing in these tests survives the container, and fsync dominates COPY
		// latency on CI disks, which is the thing being measured
		dockertest.WithCmd([]string{"postgres", "-c", "fsync=off", "-c", "full_page_writes=off"}),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		// t.Context() is canceled before cleanup funcs run, so use a non-canceled
		// context to let the container stop/remove complete.
		_ = container.Close(context.Background())
	})

	dsn := fmt.Sprintf("postgres://postgres:postgres@%s/postgres?sslmode=disable", container.GetHostPort("5432/tcp"))
	waitForPostgres(t, pool, dsn)
	t.Log("analytics postgres running on", container.GetHostPort("5432/tcp"))

	return dsn
}

// waitForPostgres blocks until the server answers a query. An open port is not
// enough: initdb starts and stops the server once before it is really up.
func waitForPostgres(t testing.TB, pool dockertest.ClosablePool, dsn string) {
	t.Helper()

	err := pool.Retry(context.Background(), 120*time.Second, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return err
		}
		defer conn.Close(context.Background())

		return conn.Ping(ctx)
	})
	require.NoError(t, err, "postgres never became ready")
}

// openAnalyticsDB returns a pool used only to verify what the sink wrote. It is
// deliberately separate from the sink's own pool so the assertions cannot be
// satisfied by anything cached in it.
func openAnalyticsDB(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()

	db, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(db.Close)

	return db
}

// dropAnalyticsSchema removes the sink's schema, both before a test so it starts on a
// clean slate and after it so a reused database does not accumulate them.
func dropAnalyticsSchema(t testing.TB, db *pgxpool.Pool, schema string) {
	t.Helper()

	drop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_, err := db.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, pgx.Identifier{schema}.Sanitize()))
		require.NoError(t, err)
	}

	drop()
	t.Cleanup(drop)
}
