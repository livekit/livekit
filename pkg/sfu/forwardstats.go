package sfu

import (
	"fmt"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

const (
	highForwardingLatency = 500 * time.Millisecond
	skewFactor            = 10
)

type ForwardStats struct {
	lock    sync.Mutex
	latency *utils.LatencyAggregate
	closeCh chan struct{}
}

func NewForwardStats(latencyUpdateInterval, reportInterval, latencyWindowLength time.Duration) *ForwardStats {
	s := &ForwardStats{
		latency: utils.NewLatencyAggregate(latencyUpdateInterval, latencyWindowLength),
		closeCh: make(chan struct{}),
	}

	go s.report(reportInterval)
	return s
}

func (s *ForwardStats) Update(arrival, left int64) (int64, bool) {
	transit := left - arrival
	isHighForwardingLatency := false
	if time.Duration(transit) > highForwardingLatency {
		isHighForwardingLatency = true
	}

	s.lock.Lock()
	s.latency.Update(time.Duration(arrival), float64(transit))
	s.lock.Unlock()

	return transit, isHighForwardingLatency
}

func (s *ForwardStats) GetStats() (time.Duration, time.Duration) {
	s.lock.Lock()
	w := s.latency.Summarize()
	s.lock.Unlock()

	latency, jitter := time.Duration(w.Mean()), time.Duration(w.StdDev())
	if jitter > latency*skewFactor {
		logger.Infow(
			"high jitter in forwarding path",
			"latency", latency,
			"jitter", jitter,
			"stats", fmt.Sprintf("count %.2f, mean %.2f, stdDev %.2f", w.Count(), w.Mean(), w.StdDev()),
		)
	}
	return latency, jitter
}

func (s *ForwardStats) GetLastStats(duration time.Duration) (time.Duration, time.Duration) {
	s.lock.Lock()
	w := s.latency.SummarizeLast(duration)
	s.lock.Unlock()

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
			prometheus.RecordForwardJitter(uint32(jitter.Microseconds()), uint32(jitterSlow.Microseconds()))
			prometheus.RecordForwardLatency(uint32(latency.Microseconds()), uint32(latencySlow.Microseconds()))
		}
	}
}
