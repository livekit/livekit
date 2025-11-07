package sfu

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/mono"
)

const (
	cHighForwardingLatency = 20 * time.Millisecond
	cSkewFactor            = 10
)

type ForwardStats struct {
	lock         sync.Mutex
	latency      *utils.LatencyAggregate
	lowest       int64
	highest      int64
	lastUpdateAt int64
	closeCh      chan struct{}
}

func NewForwardStats(latencyUpdateInterval, reportInterval, latencyWindowLength time.Duration) *ForwardStats {
	s := &ForwardStats{
		latency: utils.NewLatencyAggregate(latencyUpdateInterval, latencyWindowLength),
		lowest:  time.Second.Nanoseconds(),
		closeCh: make(chan struct{}),
	}

	go s.report(reportInterval)
	return s
}

func (s *ForwardStats) Update(arrival, left int64) (int64, bool) {
	transit := left - arrival
	isHighForwardingLatency := false
	if time.Duration(transit) > cHighForwardingLatency {
		isHighForwardingLatency = true
	}

	s.lock.Lock()
	s.latency.Update(time.Duration(arrival), float64(transit))
	s.lowest = min(transit, s.lowest)
	s.highest = max(transit, s.highest)
	s.lastUpdateAt = arrival
	s.lock.Unlock()

	return transit, isHighForwardingLatency
}

func (s *ForwardStats) GetStats(shortDuration time.Duration) (time.Duration, time.Duration, time.Duration, time.Duration) {
	s.lock.Lock()
	// a dummy sample to flush the pipe to current time
	now := mono.UnixNano()
	if (now - s.lastUpdateAt) > shortDuration.Nanoseconds() {
		s.latency.Update(time.Duration(now), 0)
	}

	wLong := s.latency.Summarize()
	wShort := s.latency.SummarizeLast(shortDuration)

	lowest := s.lowest
	s.lowest = time.Second.Nanoseconds()

	highest := s.highest
	s.highest = 0
	s.lock.Unlock()

	latencyLong, jitterLong := time.Duration(wLong.Mean()), time.Duration(wLong.StdDev())
	latencyShort, jitterShort := time.Duration(wShort.Mean()), time.Duration(wShort.StdDev())
	if latencyShort > cHighForwardingLatency/2 && jitterLong > latencyLong*cSkewFactor {
		logger.Infow(
			"high jitter in forwarding path",
			"lowest", time.Duration(lowest),
			"highest", time.Duration(highest),
			"countLong", wLong.Count(),
			"latencyLong", latencyLong,
			"jitterLong", jitterLong,
			"countShort", wShort.Count(),
			"latencyShort", latencyShort,
			"jitterShort", jitterShort,
		)
	}
	return latencyLong, jitterLong, latencyShort, jitterShort
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
			latencyLong, jitterLong, latencyShort, jitterShort := s.GetStats(reportInterval)
			prometheus.RecordForwardJitter(uint32(jitterShort.Microseconds()), uint32(jitterLong.Microseconds()))
			prometheus.RecordForwardLatency(uint32(latencyShort.Microseconds()), uint32(latencyLong.Microseconds()))
		}
	}
}
