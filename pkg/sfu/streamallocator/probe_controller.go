package streamallocator

import (
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

const (
	ProbeWaitBase      = 5 * time.Second
	ProbeBackoffFactor = 1.5
	ProbeWaitMax       = 30 * time.Second
	ProbeSettleWait    = 250
	ProbeSettleWaitMax = 10 * time.Second
	ProbeTrendWait     = 2 * time.Second

	ProbePct         = 120
	ProbeMinBps      = 200 * 1000 // 200 kbps
	ProbeMinDuration = 20 * time.Second
	ProbeMaxDuration = 21 * time.Second
)

// ---------------------------------------------------------------------------

type ProbeControllerParams struct {
	Prober *Prober
	Logger logger.Logger
}

type ProbeController struct {
	params ProbeControllerParams

	lock                  sync.RWMutex
	probeInterval         time.Duration
	lastProbeStartTime    time.Time
	probeGoalBps          int64
	probeClusterId        ProbeClusterId
	abortedProbeClusterId ProbeClusterId
	probeTrendObserved    bool
	probeEndTime          time.Time
}

func NewProbeController(params ProbeControllerParams) *ProbeController {
	p := &ProbeController{
		params: params,
	}

	p.Reset()
	return p
}

func (p *ProbeController) Reset() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.lastProbeStartTime = time.Now()

	p.resetProbeIntervalLocked()

	p.clearProbeLocked()
}

func (p *ProbeController) ProbeClusterDone(info ProbeClusterInfo, lowestEstimate int64) bool {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.probeClusterId != info.Id {
		p.params.Logger.Infow("not expected probe cluster", "probeClusterId", p.probeClusterId, "resetProbeClusterId", info.Id)
		return false
	}

	if p.abortedProbeClusterId == ProbeClusterIdInvalid {
		// successful probe, finalize
		return p.finalizeProbeLocked()
	}

	// ensure probe queue is flushed
	// STREAM-ALLOCATOR-TODO: ProbeSettleWait should actually be a certain number of RTTs.
	expectedDuration := float64(info.BytesSent*8*1000) / float64(lowestEstimate)
	queueTime := expectedDuration - float64(info.Duration.Milliseconds())
	if queueTime < 0.0 {
		queueTime = 0.0
	}
	queueWait := time.Duration(queueTime+float64(ProbeSettleWait)) * time.Millisecond
	if queueWait > ProbeSettleWaitMax {
		queueWait = ProbeSettleWaitMax
	}
	p.probeEndTime = p.lastProbeStartTime.Add(queueWait)
	p.params.Logger.Infow(
		"setting probe end time",
		"probeClusterId", p.probeClusterId,
		"expectedDuration", expectedDuration,
		"queueTime", queueTime,
		"queueWait", queueWait,
		"probeEndTime", p.probeEndTime,
	)
	return false
}

func (p *ProbeController) CheckProbe(trend ChannelTrend, highestEstimate int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.probeClusterId == ProbeClusterIdInvalid {
		return
	}

	if trend != ChannelTrendNeutral {
		p.probeTrendObserved = true
	}

	switch {
	case !p.probeTrendObserved && time.Since(p.lastProbeStartTime) > ProbeTrendWait:
		//
		// More of a safety net.
		// In rare cases, the estimate gets stuck. Prevent from probe running amok
		// STREAM-ALLOCATOR-TODO: Need more testing here to ensure that probe does not cause a lot of damage
		//
		p.params.Logger.Infow("stream allocator: probe: aborting, no trend", "cluster", p.probeClusterId)
		p.abortProbeLocked()

	case trend == ChannelTrendCongesting:
		// stop immediately if the probe is congesting channel more
		p.params.Logger.Infow("stream allocator: probe: aborting, channel is congesting", "cluster", p.probeClusterId)
		p.abortProbeLocked()

	case highestEstimate > p.probeGoalBps:
		// reached goal, stop probing
		p.params.Logger.Infow(
			"stream allocator: probe: stopping, goal reached",
			"cluster", p.probeClusterId,
			"goal", p.probeGoalBps,
			"highest", highestEstimate,
		)
		p.StopProbe()
	}
}

func (p *ProbeController) MaybeFinalizeProbe() (isHandled bool, isSuccessful bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.isInProbeLocked() && !p.probeEndTime.IsZero() && time.Now().After(p.probeEndTime) {
		return true, p.finalizeProbeLocked()
	}

	return false, false
}

func (p *ProbeController) DoesProbeNeedFinalize() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.abortedProbeClusterId != ProbeClusterIdInvalid
}

func (p *ProbeController) finalizeProbeLocked() bool {
	aborted := p.probeClusterId == p.abortedProbeClusterId

	p.clearProbeLocked()

	if aborted {
		// failed probe, backoff
		p.backoffProbeIntervalLocked()
		return false
	}

	// reset probe interval on a successful probe
	p.resetProbeIntervalLocked()
	return true
}

func (p *ProbeController) InitProbe(probeGoalDeltaBps int64, expectedBandwidthUsage int64) (ProbeClusterId, int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.lastProbeStartTime = time.Now()

	// overshoot a bit to account for noise (in measurement/estimate etc)
	p.probeGoalBps = expectedBandwidthUsage + ((probeGoalDeltaBps * ProbePct) / 100)

	p.abortedProbeClusterId = ProbeClusterIdInvalid

	p.probeTrendObserved = false

	p.probeEndTime = time.Time{}

	p.probeClusterId = p.params.Prober.AddCluster(
		ProbeClusterModeUniform,
		int(p.probeGoalBps),
		int(expectedBandwidthUsage),
		ProbeMinDuration,
		ProbeMaxDuration,
	)

	return p.probeClusterId, p.probeGoalBps
}

func (p *ProbeController) clearProbeLocked() {
	p.probeClusterId = ProbeClusterIdInvalid
	p.abortedProbeClusterId = ProbeClusterIdInvalid
}

func (p *ProbeController) backoffProbeIntervalLocked() {
	p.probeInterval = time.Duration(p.probeInterval.Seconds()*ProbeBackoffFactor) * time.Second
	if p.probeInterval > ProbeWaitMax {
		p.probeInterval = ProbeWaitMax
	}
}

func (p *ProbeController) resetProbeIntervalLocked() {
	p.probeInterval = ProbeWaitBase
}

func (p *ProbeController) StopProbe() {
	p.params.Prober.Reset()
}

func (p *ProbeController) AbortProbe() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.abortProbeLocked()
}

func (p *ProbeController) abortProbeLocked() {
	p.abortedProbeClusterId = p.probeClusterId
	p.StopProbe()
}

func (p *ProbeController) IsInProbe() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.isInProbeLocked()
}

func (p *ProbeController) isInProbeLocked() bool {
	return p.probeClusterId != ProbeClusterIdInvalid
}

func (p *ProbeController) CanProbe() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return time.Since(p.lastProbeStartTime) >= p.probeInterval && p.probeClusterId == ProbeClusterIdInvalid
}

// ------------------------------------------------
