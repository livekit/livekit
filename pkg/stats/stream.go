package stats

import (
	"math"
	"sync"
	"sync/atomic"

	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	driftBuckets = []float64{5, 10, 20, 40, 80, 160, math.Inf(+1)}

	drift = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: "rtp",
		Name:      "drift_millis",
		Buckets:   driftBuckets,
	})

	expectedCount = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "rtp",
		Name:      "expected",
	})

	receivedCount = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "rtp",
		Name:      "received",
	})

	packetCount = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "rtp",
		Name:      "packets",
	})

	totalBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "rtp",
		Name:      "bytes",
	})

	expectedMinusReceived = prometheus.NewSummary(prometheus.SummaryOpts{
		Subsystem: "rtp",
		Name:      "expected_minus_received",
	})

	lostRate = prometheus.NewSummary(prometheus.SummaryOpts{
		Subsystem: "rtp",
		Name:      "lost_rate",
	})

	jitter = prometheus.NewSummary(prometheus.SummaryOpts{
		Subsystem: "rtp",
		Name:      "jitter",
	})

	Sessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "sfu",
		Name:      "sessions",
		Help:      "Current number of sessions",
	})

	Peers = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "sfu",
		Name:      "peers",
		Help:      "Current number of peers connected",
	})

	AudioTracks = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "sfu",
		Name:      "audio_tracks",
		Help:      "Current number of audio tracks",
	})

	VideoTracks = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "sfu",
		Name:      "video_tracks",
		Help:      "Current number of video tracks",
	})
)

func InitStats() {
	prometheus.MustRegister(drift)
	prometheus.MustRegister(expectedCount)
	prometheus.MustRegister(receivedCount)
	prometheus.MustRegister(packetCount)
	prometheus.MustRegister(totalBytes)
	prometheus.MustRegister(expectedMinusReceived)
	prometheus.MustRegister(lostRate)
	prometheus.MustRegister(jitter)
	prometheus.MustRegister(Sessions)
	prometheus.MustRegister(AudioTracks)
	prometheus.MustRegister(VideoTracks)
}

// Stream contains buffer statistics
type Stream struct {
	sync.RWMutex
	Buffer        *buffer.Buffer
	cname         string
	driftInMillis uint64
	hasStats      bool
	lastStats     buffer.Stats
	diffStats     buffer.Stats
}

// NewStream constructs a new Stream
func NewStream(buffer *buffer.Buffer) *Stream {
	s := &Stream{
		Buffer: buffer,
	}
	return s
}

// GetCName returns the cname for a given stream
func (s *Stream) GetCName() string {
	s.RLock()
	defer s.RUnlock()

	return s.cname
}

func (s *Stream) SetCName(cname string) {
	s.Lock()
	defer s.Unlock()

	s.cname = cname
}

func (s *Stream) SetDriftInMillis(driftInMillis uint64) {
	atomic.StoreUint64(&s.driftInMillis, driftInMillis)
}

func (s *Stream) GetDriftInMillis() uint64 {
	return atomic.LoadUint64(&s.driftInMillis)
}

func (s *Stream) UpdateStats(stats buffer.Stats) (hasDiff bool, diffStats buffer.Stats) {
	s.Lock()
	defer s.Unlock()

	hadStats := false

	if s.hasStats {
		s.diffStats.LastExpected = stats.LastExpected - s.lastStats.LastExpected
		s.diffStats.LastReceived = stats.LastReceived - s.lastStats.LastReceived
		s.diffStats.PacketCount = stats.PacketCount - s.lastStats.PacketCount
		s.diffStats.TotalByte = stats.TotalByte - s.lastStats.TotalByte
		hadStats = true
	}

	s.lastStats = stats
	s.hasStats = true

	return hadStats, s.diffStats
}

func (s *Stream) CalcStats() {
	bufferStats := s.Buffer.GetStats()
	driftInMillis := s.GetDriftInMillis()

	hadStats, diffStats := s.UpdateStats(bufferStats)

	drift.Observe(float64(driftInMillis))
	if hadStats {
		expectedCount.Add(float64(diffStats.LastExpected))
		receivedCount.Add(float64(diffStats.LastReceived))
		packetCount.Add(float64(diffStats.PacketCount))
		totalBytes.Add(float64(diffStats.TotalByte))
	}

	expectedMinusReceived.Observe(float64(bufferStats.LastExpected - bufferStats.LastReceived))
	lostRate.Observe(float64(bufferStats.LostRate))
	jitter.Observe(bufferStats.Jitter)
}
