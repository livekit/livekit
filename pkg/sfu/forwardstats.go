package sfu

import (
	"math"
	"time"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/logger"
	"go.uber.org/atomic"
)

const (
	cHighForwardingLatency = 20 * time.Millisecond
	cSkewFactor            = 10
)

const (
	// A summary interval's worth of samples across all tracks must fit without
	// dropping (ForwardStats is a singleton). Shard count spreads the per-packet
	// atomic; shard capacity bounds memory (numShards*shardCap*8 bytes = 1MiB).
	forwardSampleNumShards = 16
	forwardSampleShardCap  = 8192
	forwardSampleShardMask = forwardSampleShardCap - 1
	forwardSampleShardSel  = forwardSampleNumShards - 1
)

// forwardSampleShard is a ring of transit samples with multiple producers and a
// single consumer. Producers reserve a slot with an atomic increment and store
// the value atomically; the consumer reads committed slots.
type forwardSampleShard struct {
	writeIdx atomic.Uint64 // advanced by producers
	readIdx  uint64        // consumer-only, no atomic needed
	ring     [forwardSampleShardCap]atomic.Int64
}

// forwardSampleBuffer holds per-packet transit samples produced on the packet
// path and consumed by the background worker, which performs metric emission.
type forwardSampleBuffer struct {
	shards  [forwardSampleNumShards]forwardSampleShard
	dropped atomic.Uint64
}

// push records a sample with one atomic add to reserve a slot and one atomic
// store to publish it. The shard is selected from arrival time bits.
func (b *forwardSampleBuffer) push(arrival, transitNs int64) {
	sh := &b.shards[(uint64(arrival)>>6)&forwardSampleShardSel]
	i := sh.writeIdx.Add(1) - 1
	sh.ring[i&forwardSampleShardMask].Store(transitNs)
}

// drain passes every committed sample to fn and advances the read cursor. Only
// the background worker calls this. When producers get more than a shard's
// capacity ahead of the cursor, the oldest excess samples are skipped and added
// to the dropped count.
func (b *forwardSampleBuffer) drain(fn func(transitNs int64)) {
	for si := range b.shards {
		sh := &b.shards[si]
		w := sh.writeIdx.Load()
		r := sh.readIdx
		if w-r > forwardSampleShardCap {
			b.dropped.Add(w - r - forwardSampleShardCap)
			r = w - forwardSampleShardCap
		}
		for ; r < w; r++ {
			fn(sh.ring[r&forwardSampleShardMask].Load())
		}
		sh.readIdx = w
	}
}

func (b *forwardSampleBuffer) takeDropped() uint64 {
	return b.dropped.Swap(0)
}

// forwardSummary is a mergeable summary of forwarding transit over an interval.
// The sum of squares is kept in microseconds so it does not overflow int64.
type forwardSummary struct {
	count   int64
	sumUs   int64
	sumSqUs int64
	minNs   int64
	maxNs   int64
}

func (s forwardSummary) addSample(transitNs int64) forwardSummary {
	us := transitNs / 1000
	if s.count == 0 {
		return forwardSummary{count: 1, sumUs: us, sumSqUs: us * us, minNs: transitNs, maxNs: transitNs}
	}

	s.count++
	s.sumUs += us
	s.sumSqUs += us * us
	if transitNs < s.minNs {
		s.minNs = transitNs
	}
	if transitNs > s.maxNs {
		s.maxNs = transitNs
	}
	return s
}

func (s forwardSummary) merge(o forwardSummary) forwardSummary {
	if o.count == 0 {
		return s
	}
	if s.count == 0 {
		return o
	}
	return forwardSummary{
		count:   s.count + o.count,
		sumUs:   s.sumUs + o.sumUs,
		sumSqUs: s.sumSqUs + o.sumSqUs,
		minNs:   min(s.minNs, o.minNs),
		maxNs:   max(s.maxNs, o.maxNs),
	}
}

func (s forwardSummary) meanStdDev() (mean, stdDev time.Duration) {
	if s.count == 0 {
		return 0, 0
	}

	meanUs := float64(s.sumUs) / float64(s.count)
	mean = time.Duration(meanUs * float64(time.Microsecond))
	if s.count < 2 {
		return mean, 0
	}

	// sample variance (divisor count-1)
	m2 := float64(s.sumSqUs) - float64(s.sumUs)*meanUs
	varUs2 := m2 / float64(s.count-1)
	if varUs2 < 0 {
		// floating point rounding can push a (near-zero) variance slightly negative
		varUs2 = 0
	}
	stdDev = time.Duration(math.Sqrt(varUs2) * float64(time.Microsecond))
	return mean, stdDev
}

type ForwardStats struct {
	samples forwardSampleBuffer

	// ring of per-summary-interval summaries covering the report window.
	// only touched by the background worker, so it needs no locking.
	ring     []forwardSummary
	ringHead int
	ringLen  int

	summaryInterval time.Duration
	reportInterval  time.Duration

	closeCh chan struct{}
}

func NewForwardStats(summaryInterval, reportInterval, reportWindow time.Duration) *ForwardStats {
	ringCap := int((reportWindow + summaryInterval - 1) / summaryInterval)
	if ringCap < 1 {
		ringCap = 1
	}

	s := &ForwardStats{
		ring:            make([]forwardSummary, ringCap),
		summaryInterval: summaryInterval,
		reportInterval:  reportInterval,
		closeCh:         make(chan struct{}),
	}

	go s.run()
	return s
}

// Update records a forwarded packet's transit latency. It buffers the sample
// and returns the transit and whether it exceeds the high-latency threshold.
// The sample is aggregated and emitted by the background worker.
func (s *ForwardStats) Update(arrival, left int64) (int64, bool) {
	transit := left - arrival
	s.samples.push(arrival, transit)
	return transit, time.Duration(transit) > cHighForwardingLatency
}

func (s *ForwardStats) Stop() {
	close(s.closeCh)
}

func (s *ForwardStats) run() {
	summaryTicker := time.NewTicker(s.summaryInterval)
	defer summaryTicker.Stop()
	reportTicker := time.NewTicker(s.reportInterval)
	defer reportTicker.Stop()

	for {
		select {
		case <-s.closeCh:
			return

		case <-summaryTicker.C:
			s.flush()

		case <-reportTicker.C:
			// fold in the most recent samples before reporting
			s.flush()
			s.report()
		}
	}
}

// flush drains the buffered samples, observes each into the Prometheus
// histogram, and folds the interval summary into the window ring used for the
// latency/jitter gauges.
func (s *ForwardStats) flush() {
	var summ forwardSummary
	s.samples.drain(func(transitNs int64) {
		prometheus.RecordForwardLatencySample(transitNs)
		summ = summ.addSample(transitNs)
	})

	s.ring[s.ringHead] = summ
	s.ringHead = (s.ringHead + 1) % len(s.ring)
	if s.ringLen < len(s.ring) {
		s.ringLen++
	}
}

func (s *ForwardStats) report() {
	var w forwardSummary
	for i := 0; i < s.ringLen; i++ {
		w = w.merge(s.ring[i])
	}

	latency, jitter := w.meanStdDev()
	if dropped := s.samples.takeDropped(); dropped > 0 {
		logger.Warnw("forward stats sample buffer overflow", nil, "dropped", dropped)
	}
	if w.count > 0 && jitter > latency*cSkewFactor {
		logger.Infow(
			"high jitter in forwarding path",
			"lowest", time.Duration(w.minNs),
			"highest", time.Duration(w.maxNs),
			"count", w.count,
			"latency", latency,
			"jitter", jitter,
		)
	}

	prometheus.RecordForwardJitter(uint32(jitter.Nanoseconds()))
	prometheus.RecordForwardLatency(uint32(latency.Nanoseconds()))
}
