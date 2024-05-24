package sfu

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/utils"
)

type ForwardStats struct {
	lock       sync.Mutex
	lastLeftMs atomic.Int64
	latency    *utils.LatencyAggregate
	closeCh    chan struct{}
}

func NewForwardStats(latencyUpdateInterval, reportInterval, latencyWindowLength time.Duration) *ForwardStats {
	s := &ForwardStats{
		latency: utils.NewLatencyAggregate(latencyUpdateInterval, latencyWindowLength),
		closeCh: make(chan struct{}),
	}

	go s.report(reportInterval)
	return s
}

func (s *ForwardStats) Update(arrival, left time.Time) {
	leftMs := left.UnixMilli()
	lastMs := s.lastLeftMs.Load()
	if leftMs < lastMs || !s.lastLeftMs.CompareAndSwap(lastMs, leftMs) {
		return
	}

	transit := left.Sub(arrival)
	s.lock.Lock()
	defer s.lock.Unlock()
	s.latency.Update(time.Duration(arrival.UnixNano()), float64(transit))
}

func (s *ForwardStats) GetStats() (latency, jitter time.Duration) {
	s.lock.Lock()
	defer s.lock.Unlock()
	w := s.latency.Summarize()
	return time.Duration(w.Mean()), time.Duration(w.StdDev())
}

func (s *ForwardStats) GetLastStats(duration time.Duration) (latency, jitter time.Duration) {
	s.lock.Lock()
	defer s.lock.Unlock()
	w := s.latency.SummarizeLast(duration)
	return time.Duration(w.Mean()), time.Duration(w.StdDev())
}

func (s *ForwardStats) Stop() {
	close(s.closeCh)
}

func (s *ForwardStats) report(reportInterval time.Duration) {
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.closeCh:
			return
		case <-ticker.C:
			latency, jitter := s.GetLastStats(reportInterval)
			latencySlow, jitterSlow := s.GetStats()
			prometheus.RecordForwardJitter(uint32(jitter/time.Millisecond), uint32(jitterSlow/time.Millisecond))
			prometheus.RecordForwardLatency(uint32(latency/time.Millisecond), uint32(latencySlow/time.Millisecond))
		}
	}
}
