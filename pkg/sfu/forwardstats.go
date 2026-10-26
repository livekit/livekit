package sfu

import (
	"math"
	"math/bits"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/logger"
	"go.uber.org/atomic"
)

const (
	cHighForwardingLatency = 20 * time.Millisecond
	cSkewFactor            = 10

	// forwardLatencyPercentile is the transit quantile reported as the node's
	// forwarding latency and read by the overload controller. p90 tracks a broad
	// slowdown but ignores the sparse tail (a few packets stalled on goroutine
	// scheduling), which shedding cannot fix.
	forwardLatencyPercentile = 0.90
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

// forwardSampleShard is a ring of packed samples with multiple producers and a
// single consumer. A producer reserves a slot, stores the value, then publishes
// the slot's epoch (reserved index + 1). The consumer reads a slot only once its
// epoch marks the value committed for that index.
type forwardSampleShard struct {
	writeIdx atomic.Uint64 // advanced by producers to reserve a slot
	readIdx  uint64        // consumer-only cursor
	ring     [forwardSampleShardCap]atomic.Int64
	seq      [forwardSampleShardCap]atomic.Uint64 // per-slot publish epoch
}

// forwardSampleBuffer holds per-packet samples produced on the packet
// path and consumed by the background worker, which performs metric emission.
type forwardSampleBuffer struct {
	shards  [forwardSampleNumShards]forwardSampleShard
	dropped atomic.Uint64
}

// push records a sample: reserve a slot, store the value, then publish the
// slot's epoch. The shard is selected from arrival time bits.
func (b *forwardSampleBuffer) push(arrival, sample int64) {
	sh := &b.shards[(uint64(arrival)>>6)&forwardSampleShardSel]
	i := sh.writeIdx.Add(1) - 1
	slot := i & forwardSampleShardMask
	sh.ring[slot].Store(sample)
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
func (b *forwardSampleBuffer) drain(fn func(sample int64)) {
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

// A sample packs transit (high 40 bits, in ns, up to ~18 minutes) and processing per
// subscriber written (low 24 bits, in 16 ns units, up to ~268 ms) into one slot, so the
// second value costs no extra atomic.
const (
	forwardProcessingBits  = 24
	forwardProcessingShift = 4 // 16 ns units
	forwardNoProcessing    = 1<<forwardProcessingBits - 1
	forwardMaxTransitNs    = 1<<(64-forwardProcessingBits) - 1
)

// packForwardSample packs a sample, a negative processingPerSubscriberNs meaning none.
func packForwardSample(transitNs, processingPerSubscriberNs int64) int64 {
	processing := uint64(forwardNoProcessing)
	if processingPerSubscriberNs >= 0 {
		processing = min(uint64(processingPerSubscriberNs)>>forwardProcessingShift, forwardNoProcessing-1)
	}
	return int64(uint64(min(max(transitNs, 0), forwardMaxTransitNs))<<forwardProcessingBits | processing)
}

// unpackForwardSample returns the transit and processing per subscriber, -1 when there is none.
func unpackForwardSample(sample int64) (transitNs, processingPerSubscriberNs int64) {
	transitNs = int64(uint64(sample) >> forwardProcessingBits)
	if processing := uint64(sample) & forwardNoProcessing; processing != forwardNoProcessing {
		return transitNs, int64(processing << forwardProcessingShift)
	}
	return transitNs, -1
}

// forwardSummary is a mergeable histogram of forwarding transit over an
// interval. Each power-of-two octave [2^e, 2^(e+1)) us is split into
// forwardHistSub linear sub-buckets, and each bucket keeps the observed
// [min, max] of the samples in it. A percentile interpolates within the
// crossing bucket's observed range rather than its nominal edges, so the
// estimate never leaves the samples and is exact when a bucket's samples
// cluster -- which keeps it accurate near an overload threshold that falls
// mid-bucket, without needing threshold-aware boundaries. Two summaries merge
// by adding their buckets, so the percentile covers the whole report window,
// and a few stalled packets (e.g. from goroutine scheduling latency) cannot
// drag it the way a mean does.
const (
	forwardHistSub     = 8                                   // linear sub-buckets per octave
	forwardHistOctaves = 27                                  // up to 2^27 us (~134 s)
	forwardHistBuckets = forwardHistOctaves * forwardHistSub // 216
)

// bucketStat is one bucket: how many samples fell in it and their observed
// transit range in nanoseconds.
type bucketStat struct {
	count int64
	minNs int64
	maxNs int64
}

func (b *bucketStat) add(transitNs int64) {
	if b.count == 0 {
		b.minNs, b.maxNs = transitNs, transitNs
	} else {
		b.minNs = min(b.minNs, transitNs)
		b.maxNs = max(b.maxNs, transitNs)
	}
	b.count++
}

func (b *bucketStat) mergeIn(o bucketStat) {
	if o.count == 0 {
		return
	}
	if b.count == 0 {
		*b = o
		return
	}
	b.count += o.count
	b.minNs = min(b.minNs, o.minNs)
	b.maxNs = max(b.maxNs, o.maxNs)
}

type forwardSummary struct {
	count   int64
	minNs   int64
	maxNs   int64
	sumNs   int64
	buckets [forwardHistBuckets]bucketStat
}

// forwardBucket returns the bucket index for a transit in nanoseconds: the
// octave floor(log2(us)) times forwardHistSub, plus the linear sub-bucket
// within the octave.
func forwardBucket(transitNs int64) int {
	us := transitNs / 1000
	if us <= 0 {
		return 0
	}
	e := bits.Len64(uint64(us)) - 1 // floor(log2(us)), us >= 1 so e >= 0
	if e >= forwardHistOctaves {
		return forwardHistBuckets - 1
	}
	octave := int64(1) << e
	return e*forwardHistSub + int((us-octave)*forwardHistSub/octave)
}

func (s *forwardSummary) addSample(transitNs int64) {
	if s.count == 0 {
		s.minNs, s.maxNs = transitNs, transitNs
	} else {
		s.minNs = min(s.minNs, transitNs)
		s.maxNs = max(s.maxNs, transitNs)
	}
	s.count++
	s.sumNs += transitNs
	s.buckets[forwardBucket(transitNs)].add(transitNs)
}

func (s forwardSummary) merge(o forwardSummary) forwardSummary {
	if o.count == 0 {
		return s
	}
	if s.count == 0 {
		return o
	}
	s.count += o.count
	s.minNs = min(s.minNs, o.minNs)
	s.maxNs = max(s.maxNs, o.maxNs)
	s.sumNs += o.sumNs
	for i := range s.buckets {
		s.buckets[i].mergeIn(o.buckets[i])
	}
	return s
}

// percentile returns the p-quantile (0..1) of transit, interpolated within the
// observed [min, max] of the bucket the quantile falls in.
func (s forwardSummary) percentile(p float64) time.Duration {
	if s.count == 0 {
		return 0
	}
	target := p * float64(s.count)
	var cum float64
	for i := range s.buckets {
		b := s.buckets[i]
		c := float64(b.count)
		if c == 0 {
			continue
		}
		if cum+c >= target {
			lo, hi := float64(b.minNs), float64(b.maxNs)
			return time.Duration(lo + (hi-lo)*(target-cum)/c)
		}
		cum += c
	}
	return time.Duration(s.maxNs)
}

func (s forwardSummary) mean() time.Duration {
	if s.count == 0 {
		return 0
	}
	return time.Duration(s.sumNs / s.count)
}

// forwardIntervalSummary holds one summary interval's transit and processing per subscriber.
type forwardIntervalSummary struct {
	transit                 forwardSummary
	processingPerSubscriber forwardSummary
}

func (s forwardIntervalSummary) merge(o forwardIntervalSummary) forwardIntervalSummary {
	return forwardIntervalSummary{
		transit:                 s.transit.merge(o.transit),
		processingPerSubscriber: s.processingPerSubscriber.merge(o.processingPerSubscriber),
	}
}

type ForwardStats struct {
	samples forwardSampleBuffer

	// ring of per-summary-interval summaries covering the report window.
	// written by the background worker (flush) and read both by the worker
	// (report) and by external callers (GetStats), so it is guarded by lock.
	lock     sync.Mutex
	ring     []forwardIntervalSummary
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
		ring:            make([]forwardIntervalSummary, ringCap),
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
	s.samples.push(arrival, packForwardSample(transit, -1))
	return transit, time.Duration(transit) > cHighForwardingLatency
}

// UpdateForwarded is Update for a packet the receiver dequeued and wrote writeCount
// times. It also records the processing (dequeue to end of fan-out) per write.
func (s *ForwardStats) UpdateForwarded(arrival, dequeuedAt, left int64, writeCount int32) (int64, bool) {
	transit := left - arrival
	processingPerSubscriber := int64(-1)
	if writeCount > 0 {
		processingPerSubscriber = (left - dequeuedAt) / int64(writeCount)
	}
	s.samples.push(arrival, packForwardSample(transit, processingPerSubscriber))
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
// histograms, and folds the interval summary into the window ring used for the
// gauges.
func (s *ForwardStats) flush() {
	var summ forwardIntervalSummary
	s.samples.drain(func(sample int64) {
		transitNs, processingPerSubscriberNs := unpackForwardSample(sample)
		prometheus.RecordForwardLatencySample(transitNs)
		summ.transit.addSample(transitNs)
		if processingPerSubscriberNs >= 0 {
			prometheus.RecordForwardProcessingPerWriteSample(processingPerSubscriberNs)
			summ.processingPerSubscriber.addSample(processingPerSubscriberNs)
		}
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
func (s *ForwardStats) summarize(window time.Duration) forwardIntervalSummary {
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
	var w forwardIntervalSummary
	for i := 0; i < n; i++ {
		idx := (s.ringHead - 1 - i + len(s.ring)) % len(s.ring)
		w = w.merge(s.ring[idx])
	}
	return w
}

// GetStats returns the reported forwarding-latency percentile
// (forwardLatencyPercentile) over the most recent duration. The duration is
// rounded up to a whole number of summary intervals (the smallest bucket span
// that covers it). A duration <= 0, or one that meets/exceeds the report window,
// covers the full window.
func (s *ForwardStats) GetStats(duration time.Duration) time.Duration {
	return s.summarize(duration).transit.percentile(forwardLatencyPercentile)
}

func (s *ForwardStats) report() {
	ws := s.summarize(0)
	w := ws.transit

	p90 := w.percentile(forwardLatencyPercentile)
	if dropped := s.samples.takeDropped(); dropped > 0 {
		logger.Warnw("forward stats sample buffer overflow", nil, "dropped", dropped)
	}
	// a max far above p90 means a few packets stalled (e.g. goroutine scheduling
	// latency) rather than a broad forwarding slowdown; p90 rides through it.
	if w.count > 0 && w.maxNs > p90.Nanoseconds()*cSkewFactor {
		logger.Infow(
			"high spread in forwarding path",
			"lowest", time.Duration(w.minNs),
			"highest", time.Duration(w.maxNs),
			"count", w.count,
			"p90", p90,
		)
	}

	prometheus.RecordForwardLatency(saturatingNs(p90))
	prometheus.RecordForwardProcessingPerWrite(saturatingNs(ws.processingPerSubscriber.mean()))
}

// saturatingNs converts to uint32 nanoseconds, saturating at ~4.29s instead of wrapping.
func saturatingNs(d time.Duration) uint32 {
	return uint32(min(max(d.Nanoseconds(), 0), math.MaxUint32))
}
