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

// Fork addition: a soak that turns recorded bytes into a number a bill can be built
// from.
//
// The accuracy tests prove the rows are right. They cannot say what the rows are
// worth, because the SFU counts RTP header plus payload and a cloud provider counts
// what leaves the interface. The difference is per packet, not per byte, so it
// depends entirely on packet size: a room full of audio pays far more overhead per
// recorded byte than a room full of video.
//
// This runs one phase per media mix and reports the ratio for each, which is the
// multiplier the rollup needs. It also holds the accuracy invariants under sustained
// load, where a retried batch or a full buffer would show up as drift.
//
//	go test ./test/ -run TestAnalyticsSinkSoak -analytics.soak=15m -timeout 30m -v
package test

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/stretchr/testify/require"

	testclient "github.com/livekit/livekit-server/test/client"
)

var (
	// -analytics.soak is both the switch and the budget: zero skips the test.
	analyticsSoakDuration = flag.Duration("analytics.soak", 0, "run the analytics soak for this long, split across its media phases")
	analyticsSoakRooms    = flag.Int("analytics.soak.rooms", 2, "concurrent rooms per soak phase")
	analyticsSoakSubs     = flag.Int("analytics.soak.subs", 3, "subscribers per soak room")
	analyticsSoakCSV      = flag.String("analytics.soak.csv", "", "write the per-phase reconciliation to this csv file")

	// The wire model. These are the bytes every packet costs that the SFU never
	// counts, and they are flags because the right values depend on the deployment.
	analyticsIPHeader  = flag.Int("analytics.wire.ip-header", 20, "ip header bytes per packet (20 for IPv4, 40 for IPv6)")
	analyticsUDPHeader = flag.Int("analytics.wire.udp-header", 8, "udp header bytes per packet")
	analyticsSRTPTag   = flag.Int("analytics.wire.srtp-tag", 10, "srtp auth tag bytes per packet (10 for AES_CM_128_HMAC_SHA1_80)")
)

// soakPhase is one media mix. Running them one after another is what separates the
// packet sizes: the node counters have no media kind label, so the only way to get a
// per-kind ratio is to measure each kind over its own window.
type soakPhase struct {
	name  string
	audio bool
	video bool
}

var soakPhases = []soakPhase{
	{name: "audio-only", audio: true},
	{name: "video-only", video: true},
	{name: "audio+video", audio: true, video: true},
}

// soakResult is everything measured for one phase, from all three views.
type soakResult struct {
	phase    soakPhase
	duration time.Duration

	recorded analyticsTotals // what the database holds, and what would be invoiced
	counted  packetCounters  // what the node counted, including packet counts
	sink     sinkCounters

	duplicateRows int64
}

// overheadPerPacket is the part of every packet that never reaches room_byte_samples.
func overheadPerPacket() int64 {
	return int64(*analyticsIPHeader + *analyticsUDPHeader + *analyticsSRTPTag)
}

func (r soakResult) overhead() int64  { return r.counted.Packets() * overheadPerPacket() }
func (r soakResult) wireBytes() int64 { return r.recorded.Bytes + r.overhead() }

func (r soakResult) ratio() float64 {
	if r.recorded.Bytes == 0 {
		return 0
	}
	return float64(r.wireBytes()) / float64(r.recorded.Bytes)
}

func (r soakResult) avgPacketBytes() float64 {
	if r.counted.Packets() == 0 {
		return 0
	}
	return float64(r.recorded.Bytes) / float64(r.counted.Packets())
}

func (r soakResult) rowsPerHour() float64 {
	if r.duration == 0 {
		return 0
	}
	return float64(r.recorded.Rows) / r.duration.Hours()
}

// TestAnalyticsSinkSoak runs each media mix in turn and reports what the recorded
// bytes actually cost.
func TestAnalyticsSinkSoak(t *testing.T) {
	if *analyticsSoakDuration <= 0 {
		t.Skip("set -analytics.soak=15m to run the analytics soak")
	}

	const schema = "livekit_analytics_soak"

	dsn := analyticsPostgres(t)
	db := openAnalyticsDB(t, dsn)
	dropAnalyticsSchema(t, db, schema)

	_, finish := setupSingleNodeTestWithConfig("TestAnalyticsSinkSoak", analyticsConfigurer(dsn, schema))
	defer finish()

	phaseDuration := *analyticsSoakDuration / time.Duration(len(soakPhases))
	t.Logf("soaking for %s total, %s per phase, %d rooms x %d subscribers",
		*analyticsSoakDuration, phaseDuration, *analyticsSoakRooms, *analyticsSoakSubs)

	results := make([]soakResult, 0, len(soakPhases))
	for _, phase := range soakPhases {
		results = append(results, runSoakPhase(t, db, schema, phase, phaseDuration))
	}

	reportSoak(t, results)
	if *analyticsSoakCSV != "" {
		writeSoakCSV(t, *analyticsSoakCSV, results)
	}
}

func runSoakPhase(t *testing.T, db *pgxpool.Pool, schema string, phase soakPhase, duration time.Duration) soakResult {
	t.Logf("--- phase %s: %s ---", phase.name, duration)

	recordedBefore := queryTotals(t, db, schema, "")
	countedBefore := gatherPacketCounters(t)
	sinkBefore := gatherSinkCounters(t)

	var (
		clients []*testclient.RTCClient
		writers []*sampleWriter
	)
	for room := range *analyticsSoakRooms {
		roomName := fmt.Sprintf("soak-%s-%d", phase.name, room)

		publisher := createAnalyticsClient(roomName, "publisher")
		clients = append(clients, publisher)
		for sub := range *analyticsSoakSubs {
			clients = append(clients, createAnalyticsClient(roomName, fmt.Sprintf("subscriber-%d", sub)))
		}
		waitUntilConnected(t, clients[len(clients)-1-*analyticsSoakSubs:]...)

		if phase.audio {
			writers = append(writers, publishSoakTrack(t, publisher, soakAudioTrack))
		}
		if phase.video {
			writers = append(writers, publishSoakTrack(t, publisher, soakVideoTrack))
		}
	}

	started := time.Now()
	for _, w := range writers {
		w.start()
	}

	// hold the invariants while the load runs, so drift is caught where it happens
	// rather than only in the totals at the end
	watchdog := time.NewTicker(telemetryFlushCadence)
	defer watchdog.Stop()
	deadline := time.After(duration)
watching:
	for {
		select {
		case <-deadline:
			break watching
		case <-watchdog.C:
			live := gatherSinkCounters(t).sub(sinkBefore)
			progress := queryTotals(t, db, schema, "")
			t.Logf("  %s elapsed: %d rows, %d B recorded, sink written %d / dropped %d / errors %d / pending %d",
				time.Since(started).Truncate(time.Second),
				progress.Rows-recordedBefore.Rows, progress.Bytes-recordedBefore.Bytes,
				live.Written, live.Dropped, live.WriteErrors, live.Pending)
			require.Zero(t, live.Dropped, "the sink dropped samples under load; billable bytes are being lost")
		}
	}

	for _, w := range writers {
		w.stop()
	}
	elapsed := time.Since(started)
	stopClients(clients...)

	countedAfter := waitForPacketCountersToSettle(t, analyticsSettleTimeout)
	counted := countedAfter.sub(countedBefore)

	// the database has to catch up with the counters before the phase can be scored
	var recorded analyticsTotals
	converged := false
	for deadline := time.Now().Add(analyticsConvergeTimeout); ; {
		totals := queryTotals(t, db, schema, "")
		recorded = analyticsTotals{
			Rows:            totals.Rows - recordedBefore.Rows,
			PrimaryBytes:    totals.PrimaryBytes - recordedBefore.PrimaryBytes,
			RetransmitBytes: totals.RetransmitBytes - recordedBefore.RetransmitBytes,
			PaddingBytes:    totals.PaddingBytes - recordedBefore.PaddingBytes,
			Bytes:           totals.Bytes - recordedBefore.Bytes,
		}
		if recorded.Bytes == counted.Bytes() {
			converged = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}

	require.True(t, converged,
		"phase %s: recorded %d B but the node counted %d B, a gap of %d B",
		phase.name, recorded.Bytes, counted.Bytes(), counted.Bytes()-recorded.Bytes)

	return soakResult{
		phase:         phase,
		duration:      elapsed,
		recorded:      recorded,
		counted:       counted,
		sink:          gatherSinkCounters(t).sub(sinkBefore),
		duplicateRows: countDuplicateKeys(t, db, schema),
	}
}

// -----------------------------------------------------------------------------
// media with a chosen packet size
// -----------------------------------------------------------------------------

// soakTrackSpec decides the average packet size, which is what the overhead ratio
// turns on. The defaults are a plain opus stream and a moderate VP8 stream; the VP8
// sample is larger than the packetizer's MTU on purpose, so it fragments the way real
// video does.
type soakTrackSpec struct {
	mimeType   string
	id         string
	sampleSize int
	period     time.Duration
}

var (
	soakAudioTrack = soakTrackSpec{mimeType: webrtc.MimeTypeOpus, id: "audio", sampleSize: 80, period: 20 * time.Millisecond}
	soakVideoTrack = soakTrackSpec{mimeType: webrtc.MimeTypeVP8, id: "video", sampleSize: 5000, period: 33 * time.Millisecond}
)

func publishSoakTrack(t *testing.T, client *testclient.RTCClient, spec soakTrackSpec) *sampleWriter {
	t.Helper()

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: spec.mimeType}, spec.id, "soak-"+spec.id)
	require.NoError(t, err)

	_, err = client.AddTrack(track, "", testclient.AddTrackNoWriter())
	require.NoError(t, err)

	return &sampleWriter{
		track:  track,
		sample: media.Sample{Data: make([]byte, spec.sampleSize), Duration: spec.period},
		period: spec.period,
	}
}

// sampleWriter feeds a track at a fixed rate. The test client's own writer only emits
// five byte samples, which would make every packet header and give a ratio no real
// deployment would ever see.
type sampleWriter struct {
	track  *webrtc.TrackLocalStaticSample
	sample media.Sample
	period time.Duration

	stopped chan struct{}
	done    chan struct{}
}

func (w *sampleWriter) start() {
	w.stopped = make(chan struct{})
	w.done = make(chan struct{})

	go func() {
		defer close(w.done)

		ticker := time.NewTicker(w.period)
		defer ticker.Stop()
		for {
			select {
			case <-w.stopped:
				return
			case <-ticker.C:
				_ = w.track.WriteSample(w.sample)
			}
		}
	}()
}

func (w *sampleWriter) stop() {
	close(w.stopped)
	<-w.done
}

// -----------------------------------------------------------------------------
// the report
// -----------------------------------------------------------------------------

func reportSoak(t *testing.T, results []soakResult) {
	t.Helper()

	out := "\n=== analytics reconciliation ===\n\n"
	out += fmt.Sprintf("wire overhead model: %d B/packet (%d ip + %d udp + %d srtp tag)\n",
		overheadPerPacket(), *analyticsIPHeader, *analyticsUDPHeader, *analyticsSRTPTag)
	out += "rtcp, stun, dtls and any turn relaying are on top of this and are not counted anywhere.\n\n"

	out += fmt.Sprintf("%-13s %12s %14s %14s %10s %12s %8s %11s\n",
		"phase", "rows", "recorded B", "packets", "avg pkt B", "overhead B", "ratio", "rows/hour")
	for _, r := range results {
		out += fmt.Sprintf("%-13s %12d %14d %14d %10.0f %12d %7.3fx %11.0f\n",
			r.phase.name, r.recorded.Rows, r.recorded.Bytes, r.counted.Packets(),
			r.avgPacketBytes(), r.overhead(), r.ratio(), r.rowsPerHour())
	}

	out += "\nper phase detail\n"
	for _, r := range results {
		out += fmt.Sprintf("  %s (%s)\n", r.phase.name, r.duration.Truncate(time.Second))
		out += fmt.Sprintf("    recorded      primary %d B, retransmit %d B, padding %d B\n",
			r.recorded.PrimaryBytes, r.recorded.RetransmitBytes, r.recorded.PaddingBytes)
		out += fmt.Sprintf("    upstream      %d B / %d pkts\n", r.counted.IncomingBytes, r.counted.IncomingPackets)
		out += fmt.Sprintf("    downstream    %d B / %d pkts\n", r.counted.OutgoingBytes, r.counted.OutgoingPackets)
		out += fmt.Sprintf("    modeled wire  %d B  (recorded x %.3f)\n", r.wireBytes(), r.ratio())
		out += fmt.Sprintf("    sink          written %d, dropped %d, errors %d, duplicate rows %d\n",
			r.sink.Written, r.sink.Dropped, r.sink.WriteErrors, r.duplicateRows)
	}

	out += "\nwhat to do with this\n"
	out += "  * the ratio column is what a recorded byte actually costs. It is not one number:\n"
	out += "    it moves with packet size, so a bill built on a single global multiplier is wrong\n"
	out += "    for every room whose media mix differs from the average.\n"
	out += "  * room_byte_samples has no packet count, so this ratio cannot be reconstructed per\n"
	out += "    room from the table alone. Recording primary/retransmit/padding packet counts\n"
	out += "    alongside the byte counts is what makes it possible.\n"
	out += "  * duplicate rows above zero means a retried COPY landed twice.\n"

	t.Log(out)
}

func writeSoakCSV(t *testing.T, path string, results []soakResult) {
	t.Helper()

	file, err := os.Create(path)
	require.NoError(t, err)
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	require.NoError(t, w.Write([]string{
		"phase", "seconds", "rows", "recorded_bytes", "primary_bytes", "retransmit_bytes", "padding_bytes",
		"upstream_bytes", "downstream_bytes", "upstream_packets", "downstream_packets",
		"overhead_bytes_per_packet", "overhead_bytes", "modeled_wire_bytes", "ratio",
		"avg_packet_bytes", "rows_per_hour", "samples_dropped", "write_errors", "duplicate_rows",
	}))

	for _, r := range results {
		require.NoError(t, w.Write([]string{
			r.phase.name,
			strconv.FormatFloat(r.duration.Seconds(), 'f', 1, 64),
			strconv.FormatInt(r.recorded.Rows, 10),
			strconv.FormatInt(r.recorded.Bytes, 10),
			strconv.FormatInt(r.recorded.PrimaryBytes, 10),
			strconv.FormatInt(r.recorded.RetransmitBytes, 10),
			strconv.FormatInt(r.recorded.PaddingBytes, 10),
			strconv.FormatInt(r.counted.IncomingBytes, 10),
			strconv.FormatInt(r.counted.OutgoingBytes, 10),
			strconv.FormatInt(r.counted.IncomingPackets, 10),
			strconv.FormatInt(r.counted.OutgoingPackets, 10),
			strconv.FormatInt(overheadPerPacket(), 10),
			strconv.FormatInt(r.overhead(), 10),
			strconv.FormatInt(r.wireBytes(), 10),
			strconv.FormatFloat(r.ratio(), 'f', 4, 64),
			strconv.FormatFloat(r.avgPacketBytes(), 'f', 1, 64),
			strconv.FormatFloat(r.rowsPerHour(), 'f', 0, 64),
			strconv.FormatInt(r.sink.Dropped, 10),
			strconv.FormatInt(r.sink.WriteErrors, 10),
			strconv.FormatInt(r.duplicateRows, 10),
		}))
	}

	t.Logf("wrote %s", path)
}
