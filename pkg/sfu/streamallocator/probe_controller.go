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
	"fmt"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

const (
	cDefaultRTT         = 70
	cRTTSmoothingFactor = float64(0.5)
)

// ---------------------------------------------------------------------------

type ProbeControllerState int

const (
	ProbeControllerStateNone ProbeControllerState = iota
	ProbeControllerStateProbing
	ProbeControllerStateHangover
)

func (p ProbeControllerState) String() string {
	switch p {
	case ProbeControllerStateNone:
		return "NONE"
	case ProbeControllerStateProbing:
		return "PROBING"
	case ProbeControllerStateHangover:
		return "HANGOVER"
	default:
		return fmt.Sprintf("%d", int(p))
	}
}

// ------------------------------------------------

type ProbeControllerConfig struct {
	BaseInterval  time.Duration `yaml:"base_interval,omitempty"`
	BackoffFactor float64       `yaml:"backoff_factor,omitempty"`
	MaxInterval   time.Duration `yaml:"max_interval,omitempty"`

	SettleWaitNumRTT uint32        `yaml:"settle_wait_num_rtt,omitempty"`
	SettleWaitMin    time.Duration `yaml:"settle_wait_min,omitempty"`
	SettleWaitMax    time.Duration `yaml:"settle_wait_max,omitempty"`

	TrendWait time.Duration `yaml:"trend_wait,omitempty"` // RAJA-REMOVE

	OveragePct             int64         `yaml:"overage_pct,omitempty"`
	MinBps                 int64         `yaml:"min_bps,omitempty"`
	MinDuration            time.Duration `yaml:"min_duration,omitempty"`
	MaxDuration            time.Duration `yaml:"max_duration,omitempty"`
	DurationIncreaseFactor float64       `yaml:"duration_increase_factor,omitempty"`
}

var (
	DefaultProbeControllerConfig = ProbeControllerConfig{
		BaseInterval:  3 * time.Second,
		BackoffFactor: 1.5,
		MaxInterval:   2 * time.Minute,

		SettleWaitNumRTT: 10,
		SettleWaitMin:    500 * time.Millisecond,
		SettleWaitMax:    10 * time.Second,

		TrendWait: 2 * time.Second,

		OveragePct:             120,
		MinBps:                 200_000,
		MinDuration:            200 * time.Millisecond,
		MaxDuration:            20 * time.Second,
		DurationIncreaseFactor: 1.5,
	}
)

// ---------------------------------------------------------------------------

type ProbeControllerParams struct {
	Config ProbeControllerConfig
	Prober *ccutils.Prober
	BWE    bwe.BWE
	Pacer  pacer.Pacer
	Logger logger.Logger
}

type ProbeController struct {
	params ProbeControllerParams

	lock            sync.RWMutex
	state           ProbeControllerState
	stateSwitchedAt time.Time

	nextProbeEarliestAt time.Time
	pci                 ccutils.ProbeClusterInfo
	rtt                 uint32
	probeInterval       time.Duration
	lastProbeStartTime  time.Time
	probeGoalBps        int64
	// RAJA-REMOVE probeClusterId            ccutils.ProbeClusterId
	doneProbeClusterInfo      ccutils.ProbeClusterInfo
	abortedProbeClusterId     ccutils.ProbeClusterId
	goalReachedProbeClusterId ccutils.ProbeClusterId
	probeTrendObserved        bool
	probeEndTime              time.Time
	probeDuration             time.Duration
}

func NewProbeController(params ProbeControllerParams) *ProbeController {
	p := &ProbeController{
		params: params,
		rtt:    cDefaultRTT,
	}

	p.Reset()
	return p
}

func (p *ProbeController) Reset() {
	/* RAJA-TODO
	p.lock.Lock()
	defer p.lock.Unlock()

	p.setState(ProbeControllerStateNone)

	p.lastProbeStartTime = mono.Now() // RAJA-TODO: is this variable needed?

	p.resetProbeIntervalLocked()
	p.resetProbeDurationLocked()

	p.StopProbe()
	p.clearProbeLocked()
	*/
}

// RAJA-TODO: need to set this
func (p *ProbeController) UpdateRTT(rtt uint32) {
	if rtt == 0 {
		p.rtt = cDefaultRTT
	} else {
		if p.rtt == 0 {
			p.rtt = rtt
		} else {
			p.rtt = uint32(cRTTSmoothingFactor*float64(rtt) + (1.0-cRTTSmoothingFactor)*float64(p.rtt))
		}
	}
}

func (p *ProbeController) MaybeProbe(availableBandwidthBps int64, probeGoalDeltaBps int64, expectedBandwidthUsage int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.state != ProbeControllerStateNone {
		// already probing or in probe hangover, don't start a new one
		return
	}

	p.setState(ProbeControllerStateProbing)

	// overshoot a bit to account for noise (in measurement/estimate etc)
	desiredIncreaseBps := (probeGoalDeltaBps * p.params.Config.OveragePct) / 100
	if desiredIncreaseBps < p.params.Config.MinBps {
		desiredIncreaseBps = p.params.Config.MinBps
	}
	probeGoalBps := expectedBandwidthUsage + desiredIncreaseBps // RAJA-REMOVE probeGoalBps struct field

	p.pci = p.params.Prober.AddCluster(
		ccutils.ProbeClusterModeUniform,
		ccutils.ProbeClusterGoal{
			AvailableBandwidthBps: int(availableBandwidthBps),
			ExpectedUsageBps:      int(expectedBandwidthUsage),
			DesiredBps:            int(probeGoalBps),
			Duration:              p.probeDuration,
		},
	)
	p.params.Logger.Debugw(
		"probe controller: starting probe",
		"probeClusterInfo", p.pci,
	)
}

func (p *ProbeController) ProbeClusterDone(pci ccutils.ProbeClusterInfo) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.pci.Id != pci.Id {
		p.params.Logger.Debugw("not expected probe cluster", "expectedId", p.pci.Id, "doneId", pci.Id)
	} else {
		p.pci.Result = pci.Result
		p.params.Prober.ClusterDone(pci)

		p.setState(ProbeControllerStateHangover)
	}
}

func (p *ProbeController) MaybeFinalizeProbe() bool {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.state != ProbeControllerStateHangover {
		return false
	}

	settleWait := time.Duration(p.params.Config.SettleWaitNumRTT*p.rtt) * time.Millisecond
	if settleWait < p.params.Config.SettleWaitMin {
		settleWait = p.params.Config.SettleWaitMin
	}
	if settleWait > p.params.Config.SettleWaitMax {
		settleWait = p.params.Config.SettleWaitMax
	}
	if time.Since(p.stateSwitchedAt) < settleWait {
		return false
	}

	p.setState(ProbeControllerStateNone)
	return true
}

/* RAJA-REMOVE
func (p *ProbeController) MaybeFinalizeProbe(
	isComplete bool,
	trend bwe.ChannelTrend,
	lowestEstimate int64,
) (isHandled bool, isNotFailing bool, isGoalReached bool) {
	// RAJA-TODO: this should just wait for hangover to get over
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
		p.doneProbeClusterInfo.Id != ccutils.ProbeClusterIdInvalid && p.doneProbeClusterInfo.Id == p.probeClusterId {
		// ensure any queueing due to probing is flushed
		// STREAM-ALLOCATOR-TODO: ProbeControllerConfig.SettleWait should actually be a certain number of RTTs.
		expectedDuration := float64(0.0)
		if lowestEstimate != 0 {
			expectedDuration = float64(p.doneProbeClusterInfo.Result.Bytes()*8*1000) / float64(lowestEstimate)
		}
		queueTime := expectedDuration - float64(p.doneProbeClusterInfo.Result.Duration().Milliseconds())
		if queueTime < 0.0 {
			queueTime = 0.0
		}
		queueWait := (time.Duration(queueTime) * time.Millisecond) + p.params.Config.SettleWait
		if queueWait > p.params.Config.SettleWaitMax {
			queueWait = p.params.Config.SettleWaitMax
		}
		p.probeEndTime = p.lastProbeStartTime.Add(queueWait + p.doneProbeClusterInfo.Result.Duration())
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
		// failed probe, backoff and restart with shorter probes
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

// RAJA-TODO: maybe call this MaybeProbe?
func (p *ProbeController) InitProbe(probeGoalDeltaBps int64, expectedBandwidthUsage int64) (ccutils.ProbeClusterId, int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.clearProbeLocked()

	p.lastProbeStartTime = time.Now()

	// overshoot a bit to account for noise (in measurement/estimate etc)
	desiredIncreaseBps := (probeGoalDeltaBps * p.params.Config.OveragePct) / 100
	if desiredIncreaseBps < p.params.Config.MinBps {
		desiredIncreaseBps = p.params.Config.MinBps
	}
	p.probeGoalBps = expectedBandwidthUsage + desiredIncreaseBps // RAJA-REMOVE probeGoalBps struct field

	p.probeClusterId = p.params.Prober.AddCluster(
		ccutils.ProbeClusterModeUniform,
		ccutils.ProbeClusterGoal{
			DesiredBps:       int(p.probeGoalBps),
			ExpectedUsageBps: int(expectedBandwidthUsage),
			Duration:         p.probeDuration,
		},
	)

	p.pollProbe(p.probeClusterId, expectedBandwidthUsage)

	return p.probeClusterId, p.probeGoalBps
}

func (p *ProbeController) pollProbe(probeClusterId ccutils.ProbeClusterId, expectedBandwidthUsage int64) {
	p.params.BWE.ProbingStart(expectedBandwidthUsage)

	go func() {
		for {
			p.lock.Lock()
			if p.probeClusterId != probeClusterId {
				p.lock.Unlock()
				return
			}

			done := false

			_, trend, _, highestEstimate := p.params.BWE.GetProbeStatus()
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
*/

/* RAJA-TODO
func (p *ProbeController) clearProbeLocked() {
	p.probeClusterId = ccutils.ProbeClusterIdInvalid
	p.abortedProbeClusterId = ccutils.ProbeClusterIdInvalid
	p.goalReachedProbeClusterId = ccutils.ProbeClusterIdInvalid

	p.doneProbeClusterInfo = ccutils.ProbeClusterInfoInvalid

	p.probeTrendObserved = false

	p.probeEndTime = time.Time{}
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
	p.params.Prober.Reset(p.params.Pacer.EndProbeCluster(p.probeClusterId))
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
*/

func (p *ProbeController) setState(state ProbeControllerState) {
	if state == p.state {
		return
	}

	p.state = state
	p.stateSwitchedAt = mono.Now()
}

// ------------------------------------------------
