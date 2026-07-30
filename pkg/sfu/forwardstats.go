package sfu

import (
	"math"
	"sync"
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
	// atomic; shard capacity bounds memory (numShards*shardCap*16 bytes = 2MiB).
	forwardSampleNumShards = 16
	forwardSampleShardCap  = 8192
	forwardSampleShardMask = forwardSampleShardCap - 1
	forwardSampleShardSel  = forwardSampleNumShards - 1
)

// forwardSampleShard is a ring of transit samples with multiple producers and a
// single consumer. A producer reserves a slot, stores the value, then publishes
// the slot's epoch (reserved index + 1). The consumer reads a slot only once its
// epoch marks the value committed for that index.
type forwardSampleShard struct {
	writeIdx atomic.Uint64 // advanced by producers to reserve a slot
	readIdx  uint64        // consumer-only cursor
	ring     [forwardSampleShardCap]atomic.Int64
	seq      [forwardSampleShardCap]atomic.Uint64 // per-slot publish epoch
}

// forwardSampleBuffer holds per-packet transit samples produced on the packet
// path and consumed by the background worker, which performs metric emission.
type forwardSampleBuffer struct {
	shards  [forwardSampleNumShards]forwardSampleShard
	dropped atomic.Uint64
}

// push records a sample: reserve a slot, store the value, then publish the
// slot's epoch. The shard is selected from arrival time bits.
func (b *forwardSampleBuffer) push(arrival, transitNs int64) {
	sh := &b.shards[(uint64(arrival)>>6)&forwardSampleShardSel]
	i := sh.writeIdx.Add(1) - 1
	slot := i & forwardSampleShardMask
	sh.ring[slot].Store(transitNs)
	sh.seq[slot].Store(i + 1)
}

// drain passes every committed sample to fn and advances the read cursor. Only
// the background worker calls this.
//
// A slot holds index r's value once its epoch equals r+1. If the slot at the
// cursor is still uncommitted (a producer reserved it but has not published),
// draining stops and resumes from there on the next call, so no sample is read
// stale or skipped. When producers get a shard's capacity ahead, or overwrite a
// slot before it is read, the affected samples are counted as dropped.
func (b *forwardSampleBuffer) drain(fn func(transitNs int64)) {
	for si := range b.shards {
		sh := &b.shards[si]
		w := sh.writeIdx.Load()
		r := sh.readIdx
		if w-r > forwardSampleShardCap {
			b.dropped.Add(w - r - forwardSampleShardCap)
			r = w - forwardSampleShardCap
		}
		for r < w {
			slot := r & forwardSampleShardMask
			if sh.seq[slot].Load() < r+1 {
				// reserved but not yet published; resume here next drain
				break
			}
			v := sh.ring[slot].Load()
			if sh.seq[slot].Load() != r+1 {
				// overwritten by a newer sample during the read; original lost
				b.dropped.Add(1)
				r++
				continue
			}
			fn(v)
			r++
		}
		sh.readIdx = r
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
	// written by the background worker (flush) and read both by the worker
	// (report) and by external callers (GetStats), so it is guarded by lock.
	lock     sync.Mutex
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
			// the summary ticker keeps the window ring current to within one
			// summary interval; report over it without advancing the ring.
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

	s.lock.Lock()
	s.ring[s.ringHead] = summ
	s.ringHead = (s.ringHead + 1) % len(s.ring)
	if s.ringLen < len(s.ring) {
		s.ringLen++
	}
	s.lock.Unlock()
}

// summarize merges the ring summaries covering the most recent window. A
// window <= 0 (or >= the report window) covers the entire ring.
func (s *ForwardStats) summarize(window time.Duration) forwardSummary {
	s.lock.Lock()
	defer s.lock.Unlock()

	n := s.ringLen
	if window > 0 && s.summaryInterval > 0 {
		want := int((window + s.summaryInterval - 1) / s.summaryInterval)
		if want < 1 {
			want = 1
		}
		if want < n {
			n = want
		}
	}

	// walk backwards from the most recent entry (ringHead-1) over n entries.
	var w forwardSummary
	for i := 0; i < n; i++ {
		idx := (s.ringHead - 1 - i + len(s.ring)) % len(s.ring)
		w = w.merge(s.ring[idx])
	}
	return w
}

// GetStats returns the mean latency and jitter (std dev) of the forwarding
// transit over the most recent duration. The duration is rounded up to a whole
// number of summary intervals (the smallest bucket span that covers it). A
// duration <= 0, or one that meets/exceeds the report window, covers the full
// window.
func (s *ForwardStats) GetStats(duration time.Duration) (time.Duration, time.Duration) {
	return s.summarize(duration).meanStdDev()
}

func (s *ForwardStats) report() {
	w := s.summarize(0)

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
