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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
)

func newTestSink(t *testing.T, bufferSize int) *pgAnalyticsService {
	t.Helper()

	conf, err := config.PostgresAnalyticsConfig{BufferSize: bufferSize, BatchSize: 1}.Resolved()
	require.NoError(t, err)

	return &pgAnalyticsService{
		conf:    conf,
		nodeID:  "ND_test",
		logger:  logger.GetLogger(),
		samples: make(chan roomByteSample, conf.BufferSize),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func TestNewAnalyticsServiceFromConfigWithoutDSNKeepsUpstreamService(t *testing.T) {
	node, err := routing.NewLocalNode(nil)
	require.NoError(t, err)

	svc, err := NewAnalyticsServiceFromConfig(&config.Config{}, node)
	require.NoError(t, err)
	require.NotNil(t, svc)
	require.IsType(t, &analyticsService{}, svc)
}

func TestNewAnalyticsServiceFromConfigRejectsUnsafeSchema(t *testing.T) {
	node, err := routing.NewLocalNode(nil)
	require.NoError(t, err)

	conf := &config.Config{Analytics: config.AnalyticsConfig{
		Postgres: config.PostgresAnalyticsConfig{
			DSN:    "postgres://user:pass@localhost:5432/hideout",
			Schema: `livekit"; DROP TABLE invoices; --`,
		},
	}}

	_, err = NewAnalyticsServiceFromConfig(conf, node)
	require.ErrorIs(t, err, config.ErrAnalyticsSchemaInvalid)
}

func TestNewAnalyticsServiceFromConfigFailsFastWhenDatabaseIsUnreachable(t *testing.T) {
	node, err := routing.NewLocalNode(nil)
	require.NoError(t, err)

	conf := &config.Config{Analytics: config.AnalyticsConfig{
		Postgres: config.PostgresAnalyticsConfig{
			// nothing listens on this port; startup must fail rather than fall back
			// to buffering, so a misconfigured/unreachable database is caught at
			// deploy time instead of silently losing billing data at runtime
			DSN:            "postgres://user:pass@127.0.0.1:1/hideout",
			ConnectTimeout: 500 * time.Millisecond,
		},
	}}

	svc, err := NewAnalyticsServiceFromConfig(conf, node)
	require.Error(t, err)
	require.Nil(t, svc)
}

func TestNewAnalyticsServiceFromConfigRejectsUnparseableDSN(t *testing.T) {
	node, err := routing.NewLocalNode(nil)
	require.NoError(t, err)

	conf := &config.Config{Analytics: config.AnalyticsConfig{
		Postgres: config.PostgresAnalyticsConfig{DSN: "postgres://user:pass@localhost:not-a-port/hideout"},
	}}

	_, err = NewAnalyticsServiceFromConfig(conf, node)
	require.Error(t, err)
}

func TestSampleFromStat(t *testing.T) {
	sink := newTestSink(t, 8)
	sampledAt := time.Date(2026, 8, 27, 10, 30, 0, 0, time.UTC)

	stat := &livekit.AnalyticsStat{
		Kind:          livekit.StreamType_DOWNSTREAM,
		TimeStamp:     timestamppb.New(sampledAt),
		RoomId:        "RM_abc",
		RoomName:      "world-org-1",
		ParticipantId: "PA_1",
		TrackId:       "TR_1",
	}
	stream := &livekit.AnalyticsStream{PrimaryBytes: 1000, RetransmitBytes: 20, PaddingBytes: 3}

	sample, ok := sink.sampleFromStat(stat, stream)
	require.True(t, ok)
	require.Equal(t, roomByteSample{
		RoomName:        "world-org-1",
		RoomID:          "RM_abc",
		ParticipantID:   "PA_1",
		TrackID:         "TR_1",
		Direction:       directionDownstream,
		PrimaryBytes:    1000,
		RetransmitBytes: 20,
		PaddingBytes:    3,
		SampledAt:       sampledAt,
		NodeID:          "ND_test",
	}, sample)
}

func TestSampleFromStatUpstreamDirection(t *testing.T) {
	sink := newTestSink(t, 8)

	sample, ok := sink.sampleFromStat(
		&livekit.AnalyticsStat{Kind: livekit.StreamType_UPSTREAM},
		&livekit.AnalyticsStream{PrimaryBytes: 1},
	)
	require.True(t, ok)
	require.Equal(t, directionUpstream, sample.Direction)
}

func TestSampleFromStatSkipsStreamsWithoutBytes(t *testing.T) {
	sink := newTestSink(t, 8)

	_, ok := sink.sampleFromStat(&livekit.AnalyticsStat{}, &livekit.AnalyticsStream{PrimaryPackets: 5})
	require.False(t, ok)
}

func TestSampleFromStatFallsBackToNowWithoutTimestamp(t *testing.T) {
	sink := newTestSink(t, 8)
	before := time.Now()

	sample, ok := sink.sampleFromStat(&livekit.AnalyticsStat{}, &livekit.AnalyticsStream{PaddingBytes: 7})
	require.True(t, ok)
	require.False(t, sample.SampledAt.Before(before))
}

func TestSendStatsBuffersOneSamplePerStream(t *testing.T) {
	sink := newTestSink(t, 8)

	sink.SendStats(context.Background(), []*livekit.AnalyticsStat{{
		Kind:     livekit.StreamType_UPSTREAM,
		RoomName: "world-org-1",
		Streams: []*livekit.AnalyticsStream{
			{PrimaryBytes: 10},
			{PrimaryBytes: 0}, // skipped, moved no bytes
			{RetransmitBytes: 5},
		},
	}})

	require.Len(t, sink.samples, 2)
}

func TestSendStatsDropsWhenBufferIsFull(t *testing.T) {
	sink := newTestSink(t, 1)
	dropped := testutil.ToFloat64(promAnalyticsSamplesDropped)

	sink.SendStats(context.Background(), []*livekit.AnalyticsStat{{
		Streams: []*livekit.AnalyticsStream{{PrimaryBytes: 1}, {PrimaryBytes: 2}, {PrimaryBytes: 3}},
	}})

	require.Len(t, sink.samples, 1)
	require.Equal(t, dropped+2, testutil.ToFloat64(promAnalyticsSamplesDropped))
}

func TestSendStatsAfterDrainIsCountedAsDropped(t *testing.T) {
	sink := newTestSink(t, 8)
	sink.stopped.Store(true)
	dropped := testutil.ToFloat64(promAnalyticsSamplesDropped)

	sink.SendStats(context.Background(), []*livekit.AnalyticsStat{{
		Streams: []*livekit.AnalyticsStream{{PrimaryBytes: 1}, {PrimaryBytes: 2}},
	}})

	require.Empty(t, sink.samples)
	require.Equal(t, dropped+2, testutil.ToFloat64(promAnalyticsSamplesDropped))
}

func TestAppendPendingEvictsOldestSamples(t *testing.T) {
	sink := newTestSink(t, 2)
	dropped := testutil.ToFloat64(promAnalyticsSamplesDropped)

	var pending []roomByteSample
	for i := int64(1); i <= 4; i++ {
		pending = sink.appendPending(pending, roomByteSample{PrimaryBytes: i})
	}

	require.Len(t, pending, 2)
	require.Equal(t, int64(3), pending[0].PrimaryBytes)
	require.Equal(t, int64(4), pending[1].PrimaryBytes)
	require.Equal(t, dropped+2, testutil.ToFloat64(promAnalyticsSamplesDropped))
}

func TestBackoffFor(t *testing.T) {
	require.Equal(t, time.Second, backoffFor(1, time.Second))
	require.Equal(t, 2*time.Second, backoffFor(2, time.Second))
	require.Equal(t, 8*time.Second, backoffFor(4, time.Second))
	require.Equal(t, maxWriteBackoff, backoffFor(30, time.Second))
	require.Equal(t, maxWriteBackoff, backoffFor(2, maxWriteBackoff))
}

func TestSinkDelegatesEverythingButStatsToUpstream(t *testing.T) {
	node, err := routing.NewLocalNode(nil)
	require.NoError(t, err)

	sink := newTestSink(t, 8)
	sink.AnalyticsService = NewAnalyticsService(&config.Config{}, node)

	// upstream returns a no-op reporter rather than nil; callers dereference it
	require.NotNil(t, sink.RoomProjectReporter(context.Background()))
	require.NotPanics(t, func() {
		sink.SendEvent(context.Background(), &livekit.AnalyticsEvent{})
		sink.SendNodeRoomStates(context.Background(), &livekit.AnalyticsNodeRooms{})
	})
}

// TestSinkPersistsStatsEndToEnd runs the wired sink against a real Postgres: it
// migrates on the writer goroutine, buffers a stats batch and drains it on shutdown.
// Skipped unless a disposable database is provided.
func TestSinkPersistsStatsEndToEnd(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(analyticsTestDSNEnv))
	if dsn == "" {
		t.Skipf("set %s to run the analytics sink integration test", analyticsTestDSNEnv)
	}

	const schema = "livekit_analytics_e2e_test"
	node, err := routing.NewLocalNode(nil)
	require.NoError(t, err)

	pgConf := config.PostgresAnalyticsConfig{DSN: dsn, Schema: schema, FlushInterval: 50 * time.Millisecond, AutoMigrate: true}
	svc, err := NewAnalyticsServiceFromConfig(&config.Config{
		Analytics: config.AnalyticsConfig{Postgres: pgConf},
	}, node)
	require.NoError(t, err)

	sink, ok := svc.(*pgAnalyticsService)
	require.True(t, ok, "a configured dsn must select the postgres sink")

	svc.SendStats(context.Background(), []*livekit.AnalyticsStat{{
		Kind:          livekit.StreamType_DOWNSTREAM,
		TimeStamp:     timestamppb.Now(),
		RoomName:      "world-org-e2e",
		RoomId:        "RM_e2e",
		ParticipantId: "PA_e2e",
		TrackId:       "TR_e2e",
		Streams:       []*livekit.AnalyticsStream{{PrimaryBytes: 500, PaddingBytes: 25}},
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sink.Drain(ctx) // flushes what is buffered, then closes the pool

	resolved, err := pgConf.Resolved()
	require.NoError(t, err)
	verify, err := newPgAnalyticsStore(resolved)
	require.NoError(t, err)
	// registered first, so it runs last: the schema is dropped while the pool is open
	t.Cleanup(verify.close)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, err := verify.pool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "livekit_analytics_e2e_test" CASCADE`)
		require.NoError(t, err)
	})

	var direction string
	var bytes int64
	require.NoError(t, verify.pool.QueryRow(ctx,
		`SELECT direction, bytes FROM "livekit_analytics_e2e_test"."room_byte_samples" WHERE room_name = $1`,
		"world-org-e2e",
	).Scan(&direction, &bytes))
	require.Equal(t, directionDownstream, direction)
	require.EqualValues(t, 525, bytes)
}
