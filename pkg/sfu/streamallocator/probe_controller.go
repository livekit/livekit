package streamallocator

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/logger"
)

// ---------------------------------------------------------------------------

type ProbeControllerParams struct {
	Config config.CongestionControlProbeConfig
	Prober *Prober
	Logger logger.Logger
}

type ProbeController struct {
	params ProbeControllerParams

	lock                      sync.RWMutex
	probeInterval             time.Duration
	lastProbeStartTime        time.Time
	probeGoalBps              int64
	probeClusterId            ProbeClusterId
	doneProbeClusterInfo      ProbeClusterInfo
	abortedProbeClusterId     ProbeClusterId
	goalReachedProbeClusterId ProbeClusterId
	probeTrendObserved        bool
	probeEndTime              time.Time
	probeDuration             time.Duration
}

func NewProbeController(params ProbeControllerParams) *ProbeController {
	p := &ProbeController{
		params:        params,
		probeDuration: params.Config.MinDuration,
	}

	p.Reset()
	return p
}

func (p *ProbeController) Reset() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.lastProbeStartTime = time.Now()

	p.resetProbeIntervalLocked()
	p.resetProbeDurationLocked()

	p.clearProbeLocked()
}

func (p *ProbeController) ProbeClusterDone(info ProbeClusterInfo) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.probeClusterId != info.Id {
		p.params.Logger.Infow("not expected probe cluster", "probeClusterId", p.probeClusterId, "resetProbeClusterId", info.Id)
		return
	}

	p.doneProbeClusterInfo = info
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
	case !p.probeTrendObserved && time.Since(p.lastProbeStartTime) > p.params.Config.TrendWait:
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
		p.goalReachedProbeClusterId = p.probeClusterId
		p.StopProbe()
	}
}

func (p *ProbeController) MaybeFinalizeProbe(
	isComplete bool,
	trend ChannelTrend,
	lowestEstimate int64,
) (isHandled bool, isNotFailing bool, isGoalReached bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.isInProbeLocked() {
		return false, false, false
	}

	if p.goalReachedProbeClusterId != ProbeClusterIdInvalid {
		// finalise goal reached probe cluster
		p.finalizeProbeLocked(ChannelTrendNeutral)
		return true, true, true
	}

	if (isComplete || p.abortedProbeClusterId != ProbeClusterIdInvalid) && p.probeEndTime.IsZero() && p.doneProbeClusterInfo.Id != ProbeClusterIdInvalid && p.doneProbeClusterInfo.Id == p.probeClusterId {
		// ensure any queueing due to probing is flushed
		// STREAM-ALLOCATOR-TODO: CongestionControlProbeConfig.SettleWait should actually be a certain number of RTTs.
		expectedDuration := float64(9.0)
		if lowestEstimate != 0 {
			expectedDuration = float64(p.doneProbeClusterInfo.BytesSent*8*1000) / float64(lowestEstimate)
		}
		queueTime := expectedDuration - float64(p.doneProbeClusterInfo.Duration.Milliseconds())
		if queueTime < 0.0 {
			queueTime = 0.0
		}
		queueWait := (time.Duration(queueTime) * time.Millisecond) + p.params.Config.SettleWait
		if queueWait > p.params.Config.SettleWaitMax {
			queueWait = p.params.Config.SettleWaitMax
		}
		p.probeEndTime = p.lastProbeStartTime.Add(queueWait + p.doneProbeClusterInfo.Duration)
		p.params.Logger.Infow(
			"setting probe end time",
			"probeClusterId", p.probeClusterId,
			"expectedDuration", expectedDuration,
			"queueTime", queueTime,
			"queueWait", queueWait,
			"probeEndTime", p.probeEndTime,
		)
	}

	if !p.probeEndTime.IsZero() && time.Now().After(p.probeEndTime) {
		// finalisze aborted or non-failing but non-goal-reached probe cluster
		return true, p.finalizeProbeLocked(trend), false
	}

	return false, false, false
}

func (p *ProbeController) DoesProbeNeedFinalize() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.abortedProbeClusterId != ProbeClusterIdInvalid || p.goalReachedProbeClusterId != ProbeClusterIdInvalid
}

func (p *ProbeController) finalizeProbeLocked(trend ChannelTrend) (isNotFailing bool) {
	aborted := p.probeClusterId == p.abortedProbeClusterId

	p.clearProbeLocked()

	if aborted || trend == ChannelTrendCongesting {
		// failed probe, backoff
		p.backoffProbeIntervalLocked()
		p.resetProbeDurationLocked()
		return false
	}

	// reset probe interval and increase probe duration on a upward trending probe
	p.resetProbeIntervalLocked()
	if trend == ChannelTrendClearing {
		p.increaseProbeDurationLocked()
	}
	return true
}

func (p *ProbeController) InitProbe(probeGoalDeltaBps int64, expectedBandwidthUsage int64) (ProbeClusterId, int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.lastProbeStartTime = time.Now()

	// overshoot a bit to account for noise (in measurement/estimate etc)
	desiredIncreaseBps := (probeGoalDeltaBps * p.params.Config.OveragePct) / 100
	if desiredIncreaseBps < p.params.Config.MinBps {
		desiredIncreaseBps = p.params.Config.MinBps
	}
	p.probeGoalBps = expectedBandwidthUsage + desiredIncreaseBps

	p.doneProbeClusterInfo = ProbeClusterInfo{Id: ProbeClusterIdInvalid}
	p.abortedProbeClusterId = ProbeClusterIdInvalid
	p.goalReachedProbeClusterId = ProbeClusterIdInvalid

	p.probeTrendObserved = false

	p.probeEndTime = time.Time{}

	p.probeClusterId = p.params.Prober.AddCluster(
		ProbeClusterModeUniform,
		int(p.probeGoalBps),
		int(expectedBandwidthUsage),
		p.probeDuration,
		time.Duration(float64(p.probeDuration.Milliseconds())*p.params.Config.DurationOverflowFactor)*time.Millisecond,
	)

	return p.probeClusterId, p.probeGoalBps
}

func (p *ProbeController) clearProbeLocked() {
	p.probeClusterId = ProbeClusterIdInvalid
	p.doneProbeClusterInfo = ProbeClusterInfo{Id: ProbeClusterIdInvalid}
	p.abortedProbeClusterId = ProbeClusterIdInvalid
	p.goalReachedProbeClusterId = ProbeClusterIdInvalid
}

func (p *ProbeController) backoffProbeIntervalLocked() {
	p.probeInterval = time.Duration(p.probeInterval.Seconds()*p.params.Config.BackoffFactor) * time.Second
	if p.probeInterval > p.params.Config.MaxInterval {
		p.probeInterval = p.params.Config.MaxInterval
	}
}

func (p *ProbeController) resetProbeIntervalLocked() {
	p.probeInterval = p.params.Config.BaseInterval
}

func (p *ProbeController) resetProbeDurationLocked() {
	p.probeDuration = p.params.Config.MinDuration
}

func (p *ProbeController) increaseProbeDurationLocked() {
	p.probeDuration = time.Duration(float64(p.probeDuration.Milliseconds())*p.params.Config.DurationIncreaseFactor) * time.Millisecond
	if p.probeDuration > p.params.Config.MaxDuration {
		p.probeDuration = p.params.Config.MaxDuration
	}
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
