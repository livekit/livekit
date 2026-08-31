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

// Fork addition: the measurements the analytics accuracy tests compare.
//
// There are three independent views of the same traffic, and the whole point of the
// end to end tests is that they have to agree:
//
//   - the database, which is what gets invoiced
//   - livekit_packet_bytes, fed from the same AnalyticsStat through a different code
//     path in pkg/telemetry/stats.go, and not gated on a stats worker existing
//   - the test client, which counts the RTP it actually received off the wire
//
// The first two must match exactly. They are integers derived from the same source,
// so any difference is a sample the sink lost, duplicated, or never saw.

package test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// packetCounters is the node level view, read from the process' prometheus
// registry. Counters are cumulative for the life of the process, so tests take a
// snapshot before and after and work with the difference.
type packetCounters struct {
	IncomingBytes   int64
	OutgoingBytes   int64
	IncomingPackets int64
	OutgoingPackets int64
}

func (p packetCounters) Bytes() int64   { return p.IncomingBytes + p.OutgoingBytes }
func (p packetCounters) Packets() int64 { return p.IncomingPackets + p.OutgoingPackets }

func (p packetCounters) sub(o packetCounters) packetCounters {
	return packetCounters{
		IncomingBytes:   p.IncomingBytes - o.IncomingBytes,
		OutgoingBytes:   p.OutgoingBytes - o.OutgoingBytes,
		IncomingPackets: p.IncomingPackets - o.IncomingPackets,
		OutgoingPackets: p.OutgoingPackets - o.OutgoingPackets,
	}
}

// sinkCounters is the sink's own health. Any non-zero Dropped means billable bytes
// were lost, whatever the other numbers say.
type sinkCounters struct {
	Written     int64
	Dropped     int64
	WriteErrors int64
	Pending     int64
}

func (s sinkCounters) sub(o sinkCounters) sinkCounters {
	return sinkCounters{
		Written:     s.Written - o.Written,
		Dropped:     s.Dropped - o.Dropped,
		WriteErrors: s.WriteErrors - o.WriteErrors,
		Pending:     s.Pending, // a gauge, not a counter: the difference is meaningless
	}
}

// gatherPacketCounters reads livekit_packet_bytes and livekit_packet_total, summed
// over every label set. The direction label is from the SFU's point of view:
// "incoming" is a participant publishing, and lines up with an 'upstream' row.
func gatherPacketCounters(t testing.TB) packetCounters {
	t.Helper()

	var c packetCounters
	eachMetric(t, "livekit_packet_bytes", func(labels map[string]string, value float64) {
		if labels["direction"] == "incoming" {
			c.IncomingBytes += int64(value)
		} else {
			c.OutgoingBytes += int64(value)
		}
	})
	eachMetric(t, "livekit_packet_total", func(labels map[string]string, value float64) {
		if labels["direction"] == "incoming" {
			c.IncomingPackets += int64(value)
		} else {
			c.OutgoingPackets += int64(value)
		}
	})

	return c
}

func gatherSinkCounters(t testing.TB) sinkCounters {
	t.Helper()

	var s sinkCounters
	for name, into := range map[string]*int64{
		"livekit_analytics_sink_samples_written_total": &s.Written,
		"livekit_analytics_sink_samples_dropped_total": &s.Dropped,
		"livekit_analytics_sink_write_errors_total":    &s.WriteErrors,
		"livekit_analytics_sink_pending_samples":       &s.Pending,
	} {
		eachMetric(t, name, func(_ map[string]string, value float64) { *into += int64(value) })
	}

	return s
}

// eachMetric calls fn for every sample of a counter or gauge family. Reading through
// the gatherer rather than the collector variables keeps these tests in the `test`
// package, where the server they measure is started.
func eachMetric(t testing.TB, name string, fn func(labels map[string]string, value float64)) {
	t.Helper()

	families, err := prom.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}

			switch {
			case metric.GetCounter() != nil:
				fn(labels, metric.GetCounter().GetValue())
			case metric.GetGauge() != nil:
				fn(labels, metric.GetGauge().GetValue())
			default:
				t.Fatalf("%s is neither a counter nor a gauge: %v", name, metric)
			}
		}
	}
}

// waitForPacketCountersToSettle blocks until the node level counters have stopped
// moving, which is how a test knows every track has emitted its final stat. Without
// it a test would be comparing a database that is still filling against a counter
// that is still rising.
func waitForPacketCountersToSettle(t testing.TB, timeout time.Duration) packetCounters {
	t.Helper()

	const (
		pollInterval = 2 * time.Second
		stillReads   = 4 // ~8s of stillness; the SFU emits stats every 5s
	)

	deadline := time.Now().Add(timeout)
	last := gatherPacketCounters(t)
	still := 0
	for {
		time.Sleep(pollInterval)

		current := gatherPacketCounters(t)
		if current == last {
			if still++; still >= stillReads {
				return current
			}
		} else {
			still = 0
			last = current
		}

		if time.Now().After(deadline) {
			t.Logf("packet counters never settled within %s, continuing with %+v", timeout, current)
			return current
		}
	}
}

// -----------------------------------------------------------------------------
// the database side
// -----------------------------------------------------------------------------

// analyticsTotals is what the sink recorded, split the way a billing policy would
// want to split it.
type analyticsTotals struct {
	Rows            int64
	PrimaryBytes    int64
	RetransmitBytes int64
	PaddingBytes    int64
	Bytes           int64
}

// queryTotals aggregates room_byte_samples. The where clause is written by the test
// and its values are always bound as parameters; the schema is a test constant.
func queryTotals(t testing.TB, db *pgxpool.Pool, schema, where string, args ...any) analyticsTotals {
	t.Helper()

	if where == "" {
		where = "true"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var totals analyticsTotals
	err := db.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*),
		       coalesce(sum(primary_bytes), 0),
		       coalesce(sum(retransmit_bytes), 0),
		       coalesce(sum(padding_bytes), 0),
		       coalesce(sum(bytes), 0)
		  FROM %s
		 WHERE %s`, analyticsTable(schema), where), args...,
	).Scan(&totals.Rows, &totals.PrimaryBytes, &totals.RetransmitBytes, &totals.PaddingBytes, &totals.Bytes)
	require.NoError(t, err)

	return totals
}

// countDuplicateKeys counts rows that share (node_id, room_id, participant_id,
// track_id, direction, sampled_at). A stats flush emits one stat per track per
// direction and stamps them all with the same instant, so that tuple should be
// unique. Anything above zero is either a retried COPY that had already committed,
// or a reason the sink cannot be made idempotent with a unique index.
func countDuplicateKeys(t testing.TB, db *pgxpool.Pool, schema string) int64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var extra int64
	err := db.QueryRow(ctx, fmt.Sprintf(`
		SELECT coalesce(sum(n - 1), 0)
		  FROM (
		       SELECT count(*) AS n
		         FROM %s
		        GROUP BY node_id, room_id, participant_id, track_id, direction, sampled_at
		       HAVING count(*) > 1
		       ) duplicated`, analyticsTable(schema)),
	).Scan(&extra)
	require.NoError(t, err)

	return extra
}

func analyticsTable(schema string) string {
	return pgx.Identifier{schema, "room_byte_samples"}.Sanitize()
}

// waitForTotals polls until want() is satisfied, so a test never has to guess how
// long a flush takes. It returns the last observed totals either way.
func waitForTotals(
	t testing.TB,
	db *pgxpool.Pool,
	schema string,
	timeout time.Duration,
	want func(analyticsTotals) bool,
) (analyticsTotals, bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var totals analyticsTotals
	for {
		totals = queryTotals(t, db, schema, "")
		if want(totals) {
			return totals, true
		}
		if time.Now().After(deadline) {
			return totals, false
		}
		time.Sleep(time.Second)
	}
}
