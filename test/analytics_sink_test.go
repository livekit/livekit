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

// Fork addition: end to end accuracy of the Postgres analytics sink.
//
// The unit tests in pkg/telemetry prove the sink does what it is told. These prove
// that what it is told is the truth: a real server, real pion clients, real media,
// and then the recorded rows are checked against two independent views of the same
// traffic - the node level prometheus counters, which are fed from the same stats
// through a different code path, and the bytes the subscribing clients actually
// received off the wire.
//
// These tests are paced by upstream telemetry, which hands stats to the sink every
// telemetryStatsUpdateInterval (30s) and is not configurable, so they take minutes
// rather than seconds. They are skipped under -short.

package test

import (
	"context"
	"flag"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
)

const (
	// analyticsSinkFlushInterval is the sink's own flush cadence. It is set far below
	// the default so the sink is never what a test is waiting for.
	analyticsSinkFlushInterval = 200 * time.Millisecond

	// telemetryFlushCadence mirrors telemetryStatsUpdateInterval in pkg/telemetry. It
	// is the real clock these tests run on: nothing reaches the sink until a stats
	// worker flushes.
	telemetryFlushCadence = 30 * time.Second

	// analyticsSettleTimeout bounds how long a test waits for every track to emit its
	// last stat after the media stops.
	analyticsSettleTimeout = 90 * time.Second

	// analyticsConvergeTimeout bounds how long the database is given to catch up with
	// the counters. It has to cover a full telemetry flush plus retries.
	analyticsConvergeTimeout = 3 * telemetryFlushCadence

	// analyticsServerPort keeps these tests off 7880/7881, so a run does not collide
	// with a livekit the developer is already running. ICE stays on ephemeral ports.
	analyticsServerPort = 7980
)

// -analytics.publish sets how long media flows before the measurement is taken. The
// default is longer than one telemetry flush on purpose, so the recorded totals are
// made of more than a single batch.
var analyticsPublishWindow = flag.Duration("analytics.publish", 40*time.Second, "how long the analytics accuracy test publishes media for")

func analyticsConfigurer(dsn, schema string) func(*config.Config) {
	return func(conf *config.Config) {
		conf.Port = analyticsServerPort
		// the ICE TCP listener is the one fixed port left; disable it rather than
		// move it, since these tests never need a TCP candidate
		conf.RTC.TCPPort = 0

		conf.Analytics.Postgres = config.PostgresAnalyticsConfig{
			DSN:           dsn,
			Schema:        schema,
			AutoMigrate:   true,
			FlushInterval: analyticsSinkFlushInterval,
		}
	}
}

func createAnalyticsClient(room, name string) *testclient.RTCClient {
	return createRTCClientWithToken(joinToken(room, name, nil), analyticsServerPort, testRTCServicePathv0, nil)
}

// TestAnalyticsSinkAccuracy is the one that answers "are the recorded bytes right".
//
// One room publishes media to three subscribers, another exchanges data messages, and
// afterwards every recorded byte is reconciled against the node counters and against
// what the subscribers received.
func TestAnalyticsSinkAccuracy(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	const (
		schema    = "livekit_analytics_accuracy"
		mediaRoom = "analytics-media"
		dataRoom  = "analytics-data"
	)

	dsn := analyticsPostgres(t)
	db := openAnalyticsDB(t, dsn)
	dropAnalyticsSchema(t, db, schema)

	packetsBefore := gatherPacketCounters(t)
	sinkBefore := gatherSinkCounters(t)

	s, finish := setupSingleNodeTestWithConfig("TestAnalyticsSinkAccuracy", analyticsConfigurer(dsn, schema))
	defer finish()
	nodeID := s.Node().Id

	// -------------------------------------------------------------------------
	// media room: one publisher, three subscribers, so the downstream total has to
	// be about three times the upstream one
	// -------------------------------------------------------------------------
	publisher := createAnalyticsClient(mediaRoom, "publisher")
	subscribers := []*testclient.RTCClient{
		createAnalyticsClient(mediaRoom, "subscriber-1"),
		createAnalyticsClient(mediaRoom, "subscriber-2"),
		createAnalyticsClient(mediaRoom, "subscriber-3"),
	}
	dataSender := createAnalyticsClient(dataRoom, "data-sender")
	dataReceiver := createAnalyticsClient(dataRoom, "data-receiver")

	allClients := append([]*testclient.RTCClient{publisher, dataSender, dataReceiver}, subscribers...)
	waitUntilConnected(t, allClients...)

	audio, err := publisher.AddStaticTrack("audio/opus", "audio", "webcamaudio")
	require.NoError(t, err)
	video, err := publisher.AddStaticTrack("video/vp8", "video", "webcamvideo")
	require.NoError(t, err)

	testutils.WithTimeout(t, func() string {
		for _, sub := range subscribers {
			if len(sub.SubscribedTracks()[publisher.ID()]) != 2 {
				return fmt.Sprintf("%s has not subscribed to both tracks yet", sub.ID())
			}
		}
		return ""
	})

	// Data traffic is recorded through the same path as media, under a
	// TR_DT<participantID> track id rather than a published track.
	//
	// It is sent lossy on purpose: a reliable channel applies backpressure, and a
	// blocked PublishData holds this goroutine long past the point the test wants to
	// stop producing.
	stopData := make(chan struct{})
	dataStopped := make(chan struct{})
	go func() {
		defer close(dataStopped)

		payload := make([]byte, 512)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopData:
				return
			case <-ticker.C:
				_ = dataSender.PublishData(payload, livekit.DataPacket_LOSSY)
			}
		}
	}()

	t.Logf("publishing for %s (telemetry flushes every %s)", *analyticsPublishWindow, telemetryFlushCadence)
	time.Sleep(*analyticsPublishWindow)

	// -------------------------------------------------------------------------
	// stop producing, then wait for the two views to converge. The server stays up:
	// stats only reach the sink on a telemetry flush, and stopping the server first
	// is a different test.
	// -------------------------------------------------------------------------
	audio.Stop()
	video.Stop()
	close(stopData)
	select {
	case <-dataStopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the data sender did not stop; PublishData is blocked on backpressure")
	}

	publisherID, subscriberIDs := publisher.ID(), make([]string, 0, len(subscribers))
	for _, sub := range subscribers {
		subscriberIDs = append(subscriberIDs, string(sub.ID()))
	}
	mediaTrackIDs := publisher.GetPublishedTrackIDs()
	require.Len(t, mediaTrackIDs, 2, "publisher should have published an audio and a video track")

	// let what is already in flight arrive before the clients stop reading, so the
	// client side ground truth is not short by a tail the server has already counted
	time.Sleep(2 * time.Second)
	stopClients(allClients...)

	subscriberBytes := make(map[livekit.ParticipantID]uint64, len(subscribers))
	for _, sub := range subscribers {
		subscriberBytes[sub.ID()] = sub.BytesReceived()
	}

	packetsAfter := waitForPacketCountersToSettle(t, analyticsSettleTimeout)
	expected := packetsAfter.sub(packetsBefore)
	require.Positive(t, expected.Bytes(), "no media was measured at all")

	recorded, converged := waitForTotals(t, db, schema, analyticsConvergeTimeout, func(totals analyticsTotals) bool {
		return totals.Bytes == expected.Bytes()
	})

	// -------------------------------------------------------------------------
	// 1. every byte the node counted was recorded, exactly once
	// -------------------------------------------------------------------------
	upstream := queryTotals(t, db, schema, "direction = 'upstream'")
	downstream := queryTotals(t, db, schema, "direction = 'downstream'")

	t.Logf("recorded %d rows, %d B total", recorded.Rows, recorded.Bytes)
	t.Logf("  upstream   %d B = primary %d + retransmit %d + padding %d",
		upstream.Bytes, upstream.PrimaryBytes, upstream.RetransmitBytes, upstream.PaddingBytes)
	t.Logf("  downstream %d B = primary %d + retransmit %d + padding %d",
		downstream.Bytes, downstream.PrimaryBytes, downstream.RetransmitBytes, downstream.PaddingBytes)
	t.Logf("node counters: incoming %d B / %d pkts, outgoing %d B / %d pkts",
		expected.IncomingBytes, expected.IncomingPackets, expected.OutgoingBytes, expected.OutgoingPackets)

	require.True(t, converged,
		"the database never caught up with the node counters: recorded %d B, counted %d B, missing %d B.\n"+
			"Every byte counted in livekit_packet_bytes was handed to TrackStats; anything that did not reach a "+
			"row was either dropped by the sink (check samples_dropped_total) or skipped by the stats worker "+
			"lookup in pkg/telemetry/stats.go, which discards a stat when the participant's worker is already gone.",
		recorded.Bytes, expected.Bytes(), expected.Bytes()-recorded.Bytes)

	// the split has to line up per direction too, not just in total. For upstream,
	// pkg/telemetry/stats.go folds retransmits into the initial counter; for
	// downstream it reports them separately. Both end up inside `bytes`.
	require.Equal(t, expected.IncomingBytes, upstream.Bytes, "upstream bytes do not match the incoming counter")
	require.Equal(t, expected.OutgoingBytes, downstream.Bytes, "downstream bytes do not match the outgoing counter")

	// -------------------------------------------------------------------------
	// 2. nothing was lost or written twice on the way
	// -------------------------------------------------------------------------
	sink := gatherSinkCounters(t).sub(sinkBefore)
	t.Logf("sink: written %d, dropped %d, write errors %d, pending %d",
		sink.Written, sink.Dropped, sink.WriteErrors, sink.Pending)

	require.Zero(t, sink.Dropped,
		"the sink dropped samples, which means billable bytes were lost.\n"+
			"Note these counters are process wide: an earlier analytics test whose sink was already drained can "+
			"contribute drops here, so re-run this test on its own before believing a small non-zero value.")
	require.Zero(t, sink.WriteErrors, "the sink failed to write a batch")
	require.Zero(t, sink.Pending, "the sink still has samples buffered")
	require.Zero(t, countDuplicateKeys(t, db, schema),
		"rows share (node_id, room_id, participant_id, track_id, direction, sampled_at), so a batch was written twice")

	// -------------------------------------------------------------------------
	// 3. the rows are attributable and internally consistent
	// -------------------------------------------------------------------------
	requireRowsAreWellFormed(t, db, schema, nodeID)

	mediaRoomTotals := queryTotals(t, db, schema, "room_name = $1", mediaRoom)
	dataRoomTotals := queryTotals(t, db, schema, "room_name = $1", dataRoom)
	require.Positive(t, mediaRoomTotals.Bytes, "no bytes recorded for the media room")
	require.Positive(t, dataRoomTotals.Bytes, "no bytes recorded for the data room")
	require.Equal(t, recorded.Bytes, mediaRoomTotals.Bytes+dataRoomTotals.Bytes,
		"some rows belong to neither room, so room_name cannot be used to attribute a bill")

	// data traffic is separable from media: it is recorded under a synthetic track id
	dataOnly := queryTotals(t, db, schema, "room_name = $1 AND track_id LIKE 'TR\\_DT%'", dataRoom)
	require.Positive(t, dataOnly.Bytes, "data channel bytes were not recorded under a TR_DT track id")

	// -------------------------------------------------------------------------
	// 4. ground truth: what the subscribers received off the wire
	// -------------------------------------------------------------------------
	//
	// This is a bound, not an equality. The SFU counts what it handed to the network;
	// the test client counts what its read loop managed to consume, and it does not
	// keep up - it drops a large share of packets before the loop sees them, so its
	// number is a floor on the truth and nothing more.
	//
	// The direction it does police is the one that matters: if a subscriber demonstrably
	// received more bytes than were recorded against it, usage went missing.
	//
	// Only primary and retransmit bytes are comparable. Padding is generated by the
	// SFU's own bandwidth probing rather than by the publisher, and pion never surfaces
	// it to the application, so it is egress that exists but is never seen.
	for _, sub := range subscribers {
		got := queryTotals(t, db, schema,
			"direction = 'downstream' AND participant_id = $1 AND track_id = ANY($2)",
			string(sub.ID()), mediaTrackIDs)

		delivered := got.PrimaryBytes + got.RetransmitBytes
		client := int64(subscriberBytes[sub.ID()])

		require.Positive(t, client, "%s received no RTP at all", sub.ID())
		require.GreaterOrEqual(t, delivered, client,
			"%s consumed %d B but only %d B was recorded against it: usage was under-recorded",
			sub.ID(), client, delivered)

		t.Logf("%s: recorded %d B delivered (+%d B padding); its read loop consumed %d B (%.0f%% of what was sent)",
			sub.ID(), delivered, got.PaddingBytes, client, float64(client)/float64(delivered)*100)
	}

	// -------------------------------------------------------------------------
	// 5. billing semantics: three subscribers cost three times one publisher
	// -------------------------------------------------------------------------
	publishedUp := queryTotals(t, db, schema,
		"direction = 'upstream' AND participant_id = $1 AND track_id = ANY($2)", string(publisherID), mediaTrackIDs)
	forwardedDown := queryTotals(t, db, schema,
		"direction = 'downstream' AND participant_id = ANY($1) AND track_id = ANY($2)", subscriberIDs, mediaTrackIDs)

	// again on delivered bytes only: padding is added per subscriber by congestion
	// control and has nothing to do with what the publisher sent
	publishedBytes := publishedUp.PrimaryBytes + publishedUp.RetransmitBytes
	forwardedBytes := forwardedDown.PrimaryBytes + forwardedDown.RetransmitBytes

	require.Positive(t, publishedBytes, "the publisher's upstream media was not recorded")
	requireWithin(t, publishedBytes*int64(len(subscribers)), forwardedBytes, 0.15,
		"forwarded media is not proportional to the number of subscribers")
}

// TestAnalyticsSinkShutdownFlush covers the last few seconds of a session, which is
// where a graceful restart either keeps or loses the tail of everyone's usage.
//
// It publishes for less than one telemetry flush and then stops the server, so
// everything the SFU measured is still held by the stats workers when the shutdown
// starts. All of it should reach the database.
func TestAnalyticsSinkShutdownFlush(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	const (
		schema = "livekit_analytics_shutdown"
		room   = "analytics-shutdown"
	)

	dsn := analyticsPostgres(t)
	db := openAnalyticsDB(t, dsn)
	dropAnalyticsSchema(t, db, schema)

	packetsBefore := gatherPacketCounters(t)

	_, finish := setupSingleNodeTestWithConfig("TestAnalyticsSinkShutdownFlush", analyticsConfigurer(dsn, schema))
	stopped := false
	defer func() {
		if !stopped {
			finish()
		}
	}()

	publisher := createAnalyticsClient(room, "publisher")
	subscriber := createAnalyticsClient(room, "subscriber")
	waitUntilConnected(t, publisher, subscriber)

	audio, err := publisher.AddStaticTrack("audio/opus", "audio", "webcamaudio")
	require.NoError(t, err)
	testutils.WithTimeout(t, func() string {
		if len(subscriber.SubscribedTracks()[publisher.ID()]) != 1 {
			return "subscriber has not subscribed yet"
		}
		return ""
	})

	// deliberately shorter than one telemetry flush: nothing has been handed to the
	// sink yet when the shutdown begins
	publishFor := telemetryFlushCadence / 3
	t.Logf("publishing for %s, which is less than one telemetry flush (%s)", publishFor, telemetryFlushCadence)
	time.Sleep(publishFor)

	audio.Stop()
	stopClients(publisher, subscriber)

	measured := waitForPacketCountersToSettle(t, analyticsSettleTimeout).sub(packetsBefore)
	require.Positive(t, measured.Bytes(), "no media was measured at all")

	finish()
	stopped = true

	recorded := queryTotals(t, db, schema, "")
	t.Logf("measured %d B before shutdown, recorded %d B after it", measured.Bytes(), recorded.Bytes)

	require.Equal(t, measured.Bytes(), recorded.Bytes,
		"usage measured before the shutdown did not survive it.\n"+
			"The sink's Drain only flushes what it has already been given. Stats are handed over by "+
			"telemetryService.FlushStats, which nothing calls on shutdown - it runs only on its 30s ticker - so "+
			"up to one flush interval of every participant's usage is still held by the stats workers when the "+
			"process exits. Calling FlushStats before draining the sink in LivekitServer.Start closes this.")
}

// requireRowsAreWellFormed asserts the invariants a billing query is entitled to
// assume about every row, whatever produced it.
func requireRowsAreWellFormed(t *testing.T, db *pgxpool.Pool, schema, nodeID string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var malformed int64
	require.NoError(t, db.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*)
		  FROM %s
		 WHERE bytes <> primary_bytes + retransmit_bytes + padding_bytes
		    OR bytes <= 0
		    OR primary_bytes < 0 OR retransmit_bytes < 0 OR padding_bytes < 0
		    OR direction NOT IN ('upstream', 'downstream')
		    OR node_id <> $1
		    OR room_name = ''
		    OR room_id = ''
		    OR sampled_at > inserted_at + interval '1 minute'`, analyticsTable(schema)), nodeID,
	).Scan(&malformed))

	require.Zero(t, malformed, "some rows cannot be billed from: check bytes, direction, node_id and room attribution")
}

// requireWithin fails unless got is within tolerance of want, and always reports the
// real difference so a run that passes still tells you how close it was.
func requireWithin(t *testing.T, want, got int64, tolerance float64, msg string) {
	t.Helper()

	diff := got - want
	var ratio float64
	if want != 0 {
		ratio = float64(diff) / float64(want)
	}

	t.Logf("%s: want %d, got %d, off by %d (%+.2f%%)", msg, want, got, diff, ratio*100)
	require.LessOrEqual(t, abs(ratio), tolerance,
		"%s: want %d, got %d, off by %+.2f%% which is more than the %.0f%% allowed",
		msg, want, got, ratio*100, tolerance*100)
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
