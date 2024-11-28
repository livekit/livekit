// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package streamallocator

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/protocol/logger"
)

// ---------------------------------------------------------------------------

type ProbeControllerConfig struct {
	BaseInterval  time.Duration `yaml:"base_interval,omitempty"`
	BackoffFactor float64       `yaml:"backoff_factor,omitempty"`
	MaxInterval   time.Duration `yaml:"max_interval,omitempty"`

	SettleWait    time.Duration `yaml:"settle_wait,omitempty"`
	SettleWaitMax time.Duration `yaml:"settle_wait_max,omitempty"`

	TrendWait time.Duration `yaml:"trend_wait,omitempty"`

	OveragePct             int64         `yaml:"overage_pct,omitempty"`
	MinBps                 int64         `yaml:"min_bps,omitempty"`
	MinDuration            time.Duration `yaml:"min_duration,omitempty"`
	MaxDuration            time.Duration `yaml:"max_duration,omitempty"`
	DurationOverflowFactor float64       `yaml:"duration_overflow_factor,omitempty"`
	DurationIncreaseFactor float64       `yaml:"duration_increase_factor,omitempty"`
}

var (
	DefaultProbeControllerConfig = ProbeControllerConfig{
		BaseInterval:  3 * time.Second,
		BackoffFactor: 1.5,
		MaxInterval:   2 * time.Minute,

		SettleWait:    250 * time.Millisecond,
		SettleWaitMax: 10 * time.Second,

		TrendWait: 2 * time.Second,

		OveragePct:             120,
		MinBps:                 200_000,
		MinDuration:            200 * time.Millisecond,
		MaxDuration:            20 * time.Second,
		DurationOverflowFactor: 1.25,
		DurationIncreaseFactor: 1.5,
	}
)

// ---------------------------------------------------------------------------

type ProbeControllerParams struct {
	Config ProbeControllerConfig
	Prober *ccutils.Prober
	Logger logger.Logger
}

type ProbeController struct {
	params ProbeControllerParams

	lock                      sync.RWMutex
	bwe                       bwe.BWE
	pacer                     pacer.Pacer
	probeInterval             time.Duration
	lastProbeStartTime        time.Time
	probeGoalBps              int64
	probeClusterId            ccutils.ProbeClusterId
	doneProbeClusterInfo      ccutils.ProbeClusterInfo
	abortedProbeClusterId     ccutils.ProbeClusterId
	goalReachedProbeClusterId ccutils.ProbeClusterId
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

func (p *ProbeController) SetBWE(bwe bwe.BWE) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.bwe = bwe
}

func (p *ProbeController) SetPacer(pacer pacer.Pacer) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.pacer = pacer
}

func (p *ProbeController) Reset() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.lastProbeStartTime = time.Now()

	p.resetProbeIntervalLocked()
	p.resetProbeDurationLocked()

	p.StopProbe()
	p.clearProbeLocked()
}

func (p *ProbeController) ProbeClusterDone(info ccutils.ProbeClusterInfo) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.probeClusterId != info.ProbeClusterId {
		p.params.Logger.Debugw("not expected probe cluster", "probeClusterId", p.probeClusterId, "resetProbeClusterId", info.ProbeClusterId)
	} else {
		p.doneProbeClusterInfo = info
	}
	// RAJA-TODO pass this on to the prober so that it can close out the cluster
}

func (p *ProbeController) MaybeFinalizeProbe(
	isComplete bool,
	trend bwe.ChannelTrend,
	lowestEstimate int64,
) (isHandled bool, isNotFailing bool, isGoalReached bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.isInProbeLocked() {
		return false, false, false
	}

	if p.goalReachedProbeClusterId != ccutils.ProbeClusterIdInvalid {
		// finalise goal reached probe cluster
		p.finalizeProbeLocked(bwe.ChannelTrendNeutral)
		return true, true, true
	}

	if (isComplete || p.abortedProbeClusterId != ccutils.ProbeClusterIdInvalid) &&
		p.probeEndTime.IsZero() &&
		p.doneProbeClusterInfo.ProbeClusterId != ccutils.ProbeClusterIdInvalid && p.doneProbeClusterInfo.ProbeClusterId == p.probeClusterId {
		// ensure any queueing due to probing is flushed
		// STREAM-ALLOCATOR-TODO: ProbeControllerConfig.SettleWait should actually be a certain number of RTTs.
		expectedDuration := float64(0.0)
		if lowestEstimate != 0 {
			expectedDuration = float64(p.doneProbeClusterInfo.Bytes()*8*1000) / float64(lowestEstimate)
		}
		queueTime := expectedDuration - float64(p.doneProbeClusterInfo.Duration().Milliseconds())
		if queueTime < 0.0 {
			queueTime = 0.0
		}
		queueWait := (time.Duration(queueTime) * time.Millisecond) + p.params.Config.SettleWait
		if queueWait > p.params.Config.SettleWaitMax {
			queueWait = p.params.Config.SettleWaitMax
		}
		p.probeEndTime = p.lastProbeStartTime.Add(queueWait + p.doneProbeClusterInfo.Duration())
		p.params.Logger.Debugw(
			"setting probe end time",
			"probeClusterId", p.probeClusterId,
			"expectedDuration", expectedDuration,
			"queueTime", queueTime,
			"queueWait", queueWait,
			"probeEndTime", p.probeEndTime,
		)
	}

	if !p.probeEndTime.IsZero() && time.Now().After(p.probeEndTime) {
		// finalize aborted or non-failing but non-goal-reached probe cluster
		return true, p.finalizeProbeLocked(trend), false
	}

	return false, false, false
}

func (p *ProbeController) DoesProbeNeedFinalize() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.abortedProbeClusterId != ccutils.ProbeClusterIdInvalid || p.goalReachedProbeClusterId != ccutils.ProbeClusterIdInvalid
}

func (p *ProbeController) finalizeProbeLocked(trend bwe.ChannelTrend) (isNotFailing bool) {
	aborted := p.probeClusterId == p.abortedProbeClusterId

	p.clearProbeLocked()

	if aborted || trend == bwe.ChannelTrendCongesting {
		// failed probe, backoff
		p.backoffProbeIntervalLocked()
		p.resetProbeDurationLocked()
		return false
	}

	// reset probe interval and increase probe duration on a upward trending probe
	p.resetProbeIntervalLocked()
	if trend == bwe.ChannelTrendClearing {
		p.increaseProbeDurationLocked()
	}
	return true
}

func (p *ProbeController) InitProbe(probeGoalDeltaBps int64, expectedBandwidthUsage int64) (ccutils.ProbeClusterId, int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.lastProbeStartTime = time.Now()

	// overshoot a bit to account for noise (in measurement/estimate etc)
	desiredIncreaseBps := (probeGoalDeltaBps * p.params.Config.OveragePct) / 100
	if desiredIncreaseBps < p.params.Config.MinBps {
		desiredIncreaseBps = p.params.Config.MinBps
	}
	p.probeGoalBps = expectedBandwidthUsage + desiredIncreaseBps

	p.doneProbeClusterInfo = ccutils.ProbeClusterInfoInvalid
	p.abortedProbeClusterId = ccutils.ProbeClusterIdInvalid
	p.goalReachedProbeClusterId = ccutils.ProbeClusterIdInvalid

	p.probeTrendObserved = false

	p.probeEndTime = time.Time{}

	p.probeClusterId = p.params.Prober.AddCluster(
		ccutils.ProbeClusterModeUniform,
		int(p.probeGoalBps),
		int(expectedBandwidthUsage),
		p.probeDuration,
		time.Duration(float64(p.probeDuration.Milliseconds())*p.params.Config.DurationOverflowFactor)*time.Millisecond,
	)

	p.pollProbe(p.probeClusterId, expectedBandwidthUsage)

	return p.probeClusterId, p.probeGoalBps
}

func (p *ProbeController) pollProbe(probeClusterId ccutils.ProbeClusterId, expectedBandwidthUsage int64) {
	if p.bwe == nil {
		return
	}

	p.bwe.ProbingStart(expectedBandwidthUsage)

	go func() {
		for {
			p.lock.Lock()
			if p.probeClusterId != probeClusterId {
				p.lock.Unlock()
				return
			}

			done := false

			_, trend, _, highestEstimate := p.bwe.GetProbeStatus()
			if !p.probeTrendObserved && trend != bwe.ChannelTrendNeutral {
				p.probeTrendObserved = true
			}

			switch {
			case trend == bwe.ChannelTrendCongesting:
				// stop immediately if the probe is congesting channel more
				p.params.Logger.Infow(
					"stream allocator: probe: aborting, channel is congesting",
					"cluster", probeClusterId,
				)
				p.abortProbeLocked()
				done = true
				break

			case highestEstimate > p.probeGoalBps:
				// reached goal, stop probing
				p.params.Logger.Infow(
					"stream allocator: probe: stopping, goal reached",
					"cluster", probeClusterId,
					"goal", p.probeGoalBps,
					"highestEstimate", highestEstimate,
				)
				p.goalReachedProbeClusterId = p.probeClusterId
				p.StopProbe()
				done = true
				break

			case !p.probeTrendObserved && time.Since(p.lastProbeStartTime) > p.params.Config.TrendWait:
				//
				// More of a safety net.
				// In rare cases, the estimate gets stuck. Prevent from probe running amok
				// STREAM-ALLOCATOR-TODO: Need more testing here to ensure that probe does not cause a lot of damage
				//
				p.params.Logger.Infow("stream allocator: probe: aborting, no trend", "cluster", probeClusterId)
				p.abortProbeLocked()
				done = true
				break
			}
			p.lock.Unlock()

			if done {
				return
			}

			// BWE-TODO: do not hard code sleep time
			time.Sleep(50 * time.Millisecond)
		}
	}()
}

func (p *ProbeController) clearProbeLocked() {
	p.probeClusterId = ccutils.ProbeClusterIdInvalid
	p.doneProbeClusterInfo = ccutils.ProbeClusterInfoInvalid
	p.abortedProbeClusterId = ccutils.ProbeClusterIdInvalid
	p.goalReachedProbeClusterId = ccutils.ProbeClusterIdInvalid
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
	info := ccutils.ProbeClusterInfoInvalid
	if p.pacer != nil {
		info = p.pacer.EndProbeCluster(p.probeClusterId)
	}
	p.params.Prober.Reset(info)
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

func (p *ProbeController) isInProbeLocked() bool {
	return p.probeClusterId != ccutils.ProbeClusterIdInvalid
}

func (p *ProbeController) CanProbe() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return time.Since(p.lastProbeStartTime) >= p.probeInterval && p.probeClusterId == ccutils.ProbeClusterIdInvalid
}

// ------------------------------------------------
