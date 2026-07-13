package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
)

// initPrometheus initializes the global forward-latency collectors so that the
// worker's metric emission has non-nil targets. Init returns early if already
// initialized, so it is safe to call from multiple tests.
func initPrometheus(t *testing.T) {
	t.Helper()
	require.NoError(t, prometheus.Init("test", livekit.NodeType_SERVER))
}

// ---------------------------------------------------------------------------
// forwardSummary
// ---------------------------------------------------------------------------

func TestForwardSummary_AddSample(t *testing.T) {
	var s forwardSummary

	// empty summary
	require.Equal(t, int64(0), s.count)

	// microsecond-aligned transits so the /1000 truncation is exact
	s = s.addSample(3000) // 3us
	s = s.addSample(1000) // 1us
	s = s.addSample(2000) // 2us

	require.Equal(t, int64(3), s.count)
	require.Equal(t, int64(1+2+3), s.sumUs)
	require.Equal(t, int64(1+4+9), s.sumSqUs)
	require.Equal(t, int64(1000), s.minNs)
	require.Equal(t, int64(3000), s.maxNs)
}

func TestForwardSummary_Merge(t *testing.T) {
	var empty forwardSummary
	a := forwardSummary{}.addSample(1000).addSample(2000)
	b := forwardSummary{}.addSample(5000).addSample(3000)

	// merging with empty is identity, in both directions
	require.Equal(t, a, a.merge(empty))
	require.Equal(t, a, empty.merge(a))

	m := a.merge(b)
	require.Equal(t, int64(4), m.count)
	require.Equal(t, a.sumUs+b.sumUs, m.sumUs)
	require.Equal(t, a.sumSqUs+b.sumSqUs, m.sumSqUs)
	require.Equal(t, int64(1000), m.minNs)
	require.Equal(t, int64(5000), m.maxNs)
}

func TestForwardSummary_MeanStdDev(t *testing.T) {
	// empty -> zero
	mean, stdDev := forwardSummary{}.meanStdDev()
	require.Zero(t, mean)
	require.Zero(t, stdDev)

	// single sample -> mean set, stddev zero (needs >= 2 for variance)
	mean, stdDev = forwardSummary{}.addSample(4000).meanStdDev()
	require.Equal(t, 4*time.Microsecond, mean)
	require.Zero(t, stdDev)

	// identical samples -> zero variance
	s := forwardSummary{}.addSample(2000).addSample(2000).addSample(2000)
	mean, stdDev = s.meanStdDev()
	require.Equal(t, 2*time.Microsecond, mean)
	require.Zero(t, stdDev)

	// known dataset [1us, 2us, 3us]: mean 2us, sample variance 1us^2 -> stddev 1us
	s = forwardSummary{}.addSample(1000).addSample(2000).addSample(3000)
	mean, stdDev = s.meanStdDev()
	require.Equal(t, 2*time.Microsecond, mean)
	require.InDelta(t, float64(time.Microsecond), float64(stdDev), float64(50*time.Nanosecond))
}

// ---------------------------------------------------------------------------
// forwardSampleBuffer
// ---------------------------------------------------------------------------

func shardOf(arrival int64) int {
	return int((uint64(arrival) >> 6) & forwardSampleShardSel)
}

// arrivalForShard returns the n-th arrival value that maps to a fixed shard.
// Incrementing arrival by (1<<10) advances (arrival>>6) by 16, leaving the low
// 4 selection bits unchanged.
func arrivalForShard(n int) int64 {
	return int64(n) << 10
}

func TestForwardSampleBuffer_PushDrain(t *testing.T) {
	var b forwardSampleBuffer

	const n = 1000
	for i := 0; i < n; i++ {
		b.push(int64(i), int64((i+1)*1000))
	}

	got := map[int64]int{}
	total := 0
	b.drain(func(v int64) {
		got[v]++
		total++
	})
	require.Equal(t, n, total)
	require.Equal(t, uint64(0), b.dropped.Load())
	for i := 0; i < n; i++ {
		require.Equal(t, 1, got[int64((i+1)*1000)], "sample %d missing", i)
	}

	// draining again yields nothing (read cursor advanced)
	total = 0
	b.drain(func(v int64) { total++ })
	require.Equal(t, 0, total)
}

func TestForwardSampleBuffer_Overflow(t *testing.T) {
	var b forwardSampleBuffer

	const extra = 100
	const n = forwardSampleShardCap + extra

	// pin every push to a single shard so it overflows
	for i := 0; i < n; i++ {
		b.push(arrivalForShard(i), int64(i)*1000)
	}
	require.Equal(t, 0, shardOf(arrivalForShard(0)))
	require.Equal(t, shardOf(arrivalForShard(0)), shardOf(arrivalForShard(n-1)))

	var drained []int64
	b.drain(func(v int64) { drained = append(drained, v) })

	// exactly a shard's worth survives; the oldest `extra` are dropped and counted
	require.Len(t, drained, forwardSampleShardCap)
	require.Equal(t, uint64(extra), b.dropped.Load())

	// survivors are the most recent cap samples, in order
	for j, v := range drained {
		require.Equal(t, int64(extra+j)*1000, v)
	}
}

func TestForwardSampleBuffer_DefersUncommitted(t *testing.T) {
	var b forwardSampleBuffer
	sh := &b.shards[0]

	// simulate a producer that reserved index 0 but has not published its value
	sh.writeIdx.Store(1)

	got := 0
	b.drain(func(int64) { got++ })
	require.Equal(t, 0, got, "uncommitted slot must not be read")
	require.Equal(t, uint64(0), sh.readIdx, "cursor must not advance past an uncommitted slot")
	require.Equal(t, uint64(0), b.dropped.Load())

	// producer publishes the value; next drain picks it up
	sh.ring[0].Store(1234)
	sh.seq[0].Store(1)

	var vals []int64
	b.drain(func(v int64) { vals = append(vals, v) })
	require.Equal(t, []int64{1234}, vals)
	require.Equal(t, uint64(1), sh.readIdx)
	require.Equal(t, uint64(0), b.dropped.Load())
}

func TestForwardSampleBuffer_Concurrent(t *testing.T) {
	var b forwardSampleBuffer
	var stop atomic.Bool

	var consumed int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			b.drain(func(int64) { consumed++ })
			time.Sleep(time.Millisecond)
		}
		b.drain(func(int64) { consumed++ }) // final sweep
	}()

	const producers = 8
	const perProducer = 100_000
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			for i := int64(0); i < perProducer; i++ {
				b.push(seed*7+i, (i%50)*int64(time.Microsecond))
			}
		}(int64(p))
	}
	wg.Wait()
	stop.Store(true)
	<-done

	// with a consumer keeping pace no samples should be lost
	require.Equal(t, int64(producers*perProducer), consumed+int64(b.dropped.Load()))
}

// ---------------------------------------------------------------------------
// ForwardStats
// ---------------------------------------------------------------------------

func TestForwardStats_Update(t *testing.T) {
	s := &ForwardStats{ring: make([]forwardSummary, 1)}

	// below threshold
	transit, isHigh := s.Update(1000, 1000+int64(5*time.Millisecond))
	require.Equal(t, int64(5*time.Millisecond), transit)
	require.False(t, isHigh)

	// above threshold
	transit, isHigh = s.Update(1000, 1000+int64(25*time.Millisecond))
	require.Equal(t, int64(25*time.Millisecond), transit)
	require.True(t, isHigh)

	// exactly at threshold is not "high" (strictly greater)
	_, isHigh = s.Update(0, int64(cHighForwardingLatency))
	require.False(t, isHigh)
}

func TestForwardStats_Flush(t *testing.T) {
	initPrometheus(t)

	s := &ForwardStats{ring: make([]forwardSummary, 4)}

	for i := 0; i < 10; i++ {
		s.Update(0, int64((i+1)*1000)) // 1us..10us
	}

	s.flush()

	require.Equal(t, 1, s.ringLen)
	summ := s.ring[0]
	require.Equal(t, int64(10), summ.count)
	require.Equal(t, int64(1000), summ.minNs)
	require.Equal(t, int64(10000), summ.maxNs)
	require.Equal(t, uint64(0), s.samples.dropped.Load())

	// a subsequent flush with no new samples appends an empty summary
	s.flush()
	require.Equal(t, 2, s.ringLen)
	require.Equal(t, int64(0), s.ring[1].count)
}

func TestForwardStats_ReportWindow(t *testing.T) {
	initPrometheus(t)

	// window of 3 summary buckets
	s := &ForwardStats{ring: make([]forwardSummary, 3)}

	s.Update(0, 1000)
	s.flush()
	s.Update(0, 3000)
	s.flush()

	// report merges the whole window without panicking and reflects both samples
	var w forwardSummary
	for i := 0; i < s.ringLen; i++ {
		w = w.merge(s.ring[i])
	}
	require.Equal(t, int64(2), w.count)
	require.Equal(t, int64(1000), w.minNs)
	require.Equal(t, int64(3000), w.maxNs)

	require.NotPanics(t, s.report)
}

func TestForwardStats_GetStats(t *testing.T) {
	initPrometheus(t)

	// 5 buckets, each covering one 100ms summary interval.
	s := &ForwardStats{ring: make([]forwardSummary, 5), summaryInterval: 100 * time.Millisecond}

	// fold five 100ms buckets, one sample each: 1ms, 2ms, 3ms, 4ms, 5ms.
	for i := 1; i <= 5; i++ {
		s.Update(0, int64(i)*int64(time.Millisecond))
		s.flush()
	}
	require.Equal(t, 5, s.ringLen)

	// a duration <= 0 covers the whole window: mean of 1..5ms == 3ms.
	latency, jitter := s.GetStats(0)
	require.InDelta(t, float64(3*time.Millisecond), float64(latency), float64(50*time.Microsecond))
	require.Greater(t, jitter, time.Duration(0))

	// a duration meeting/exceeding the window also covers it.
	fullLatency, _ := s.GetStats(time.Second)
	require.InDelta(t, float64(3*time.Millisecond), float64(fullLatency), float64(50*time.Microsecond))

	// ~200ms rounds up to the two most recent buckets (4ms, 5ms): mean == 4.5ms.
	shortLatency, _ := s.GetStats(200 * time.Millisecond)
	require.InDelta(t, float64(4500*time.Microsecond), float64(shortLatency), float64(50*time.Microsecond))

	// a sub-interval duration still yields at least the most recent bucket (5ms).
	lastLatency, _ := s.GetStats(time.Nanosecond)
	require.InDelta(t, float64(5*time.Millisecond), float64(lastLatency), float64(50*time.Microsecond))
}

func TestForwardStats_Lifecycle(t *testing.T) {
	initPrometheus(t)

	s := NewForwardStats(5*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond)
	for i := 0; i < 1000; i++ {
		s.Update(int64(i), int64(i)+int64(time.Millisecond))
	}
	time.Sleep(60 * time.Millisecond) // let the worker flush/report a few times
	require.NotPanics(t, s.Stop)
}

func TestNewForwardStats_RingSizing(t *testing.T) {
	// ringCap = ceil(window / summaryInterval)
	s := NewForwardStats(100*time.Millisecond, time.Second, time.Second)
	require.Equal(t, 10, len(s.ring))
	s.Stop()

	// rounds up a partial interval
	s = NewForwardStats(100*time.Millisecond, time.Second, 250*time.Millisecond)
	require.Equal(t, 3, len(s.ring))
	s.Stop()

	// never smaller than one bucket, even if window < summaryInterval
	s = NewForwardStats(time.Second, time.Second, 100*time.Millisecond)
	require.Equal(t, 1, len(s.ring))
	s.Stop()
}

// ---------------------------------------------------------------------------
// benchmark: per-packet cost of Update (run with -cpu 1,8).
// ---------------------------------------------------------------------------

// benchArrival advances the arrival timestamp by 64ns per packet so that
// consecutive packets from one goroutine map to successive shards
// ((arrival>>6)&mask increments each step). A distinct per-goroutine base
// spreads goroutines across shards.
func benchArrival(base, i int64) int64 {
	return base + i*64
}

func BenchmarkForwardStatsUpdate(b *testing.B) {
	s := &ForwardStats{ring: make([]forwardSummary, 1)}

	var gid atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		base := gid.Add(1) * 1_000_003
		var i int64
		for pb.Next() {
			i++
			arrival := benchArrival(base, i)
			s.Update(arrival, arrival+int64(2*time.Millisecond))
		}
	})
}
