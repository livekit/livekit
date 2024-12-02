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

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

const (
	cDefaultRTT         = float64(0.070) // 70 ms
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

	OveragePct int64 `yaml:"overage_pct,omitempty"`
	MinBps     int64 `yaml:"min_bps,omitempty"`

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

		OveragePct: 120,
		MinBps:     200_000,

		MinDuration:            200 * time.Millisecond,
		MaxDuration:            20 * time.Second,
		DurationIncreaseFactor: 1.5,
	}
)

// ---------------------------------------------------------------------------

type ProbeControllerParams struct {
	Config ProbeControllerConfig
	Logger logger.Logger
}

type ProbeController struct {
	params ProbeControllerParams

	lock sync.RWMutex

	state           ProbeControllerState
	stateSwitchedAt time.Time

	pci ccutils.ProbeClusterInfo
	rtt float64

	probeInterval       time.Duration
	probeDuration       time.Duration
	nextProbeEarliestAt time.Time
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
	p.lock.Lock()
	defer p.lock.Unlock()

	p.state = ProbeControllerStateNone
	p.stateSwitchedAt = mono.Now()
	p.pci = ccutils.ProbeClusterInfoInvalid
	p.probeInterval = p.params.Config.BaseInterval
	p.probeDuration = p.params.Config.MinDuration
	p.nextProbeEarliestAt = mono.Now()
}

func (p *ProbeController) UpdateRTT(rtt float64) {
	if rtt == 0 {
		p.rtt = cDefaultRTT
	} else {
		if p.rtt == 0 {
			p.rtt = rtt
		} else {
			p.rtt = cRTTSmoothingFactor*rtt + (1.0-cRTTSmoothingFactor)*p.rtt
		}
	}
}

func (p *ProbeController) CanProbe() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.state == ProbeControllerStateNone && mono.Now().After(p.nextProbeEarliestAt)
}

func (p *ProbeController) MaybeInitiateProbe(availableBandwidthBps int64, probeGoalDeltaBps int64, expectedBandwidthUsage int64) (ccutils.ProbeClusterGoal, bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if p.state != ProbeControllerStateNone {
		// already probing or in probe hangover, don't start a new one
		return ccutils.ProbeClusterGoal{}, false
	}

	if mono.Now().Before(p.nextProbeEarliestAt) {
		return ccutils.ProbeClusterGoal{}, false
	}

	// overshoot a bit to account for noise (in measurement/estimate etc)
	desiredIncreaseBps := (probeGoalDeltaBps * p.params.Config.OveragePct) / 100
	if desiredIncreaseBps < p.params.Config.MinBps {
		desiredIncreaseBps = p.params.Config.MinBps
	}
	return ccutils.ProbeClusterGoal{
		AvailableBandwidthBps: int(availableBandwidthBps),
		ExpectedUsageBps:      int(expectedBandwidthUsage),
		DesiredBps:            int(expectedBandwidthUsage + desiredIncreaseBps),
		Duration:              p.probeDuration,
	}, true
}

func (p *ProbeController) ProbeClusterStarting(pci ccutils.ProbeClusterInfo) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.state != ProbeControllerStateNone {
		p.params.Logger.Warnw("unexpected probe controller state", nil, "state", p.state)
	}

	p.setState(ProbeControllerStateProbing)
	p.pci = pci
}

func (p *ProbeController) ProbeClusterDone(pci ccutils.ProbeClusterInfo) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.pci.Id == pci.Id {
		p.pci.Result = pci.Result
		p.setState(ProbeControllerStateHangover)
	}
}

func (p *ProbeController) MaybeFinalizeProbe() (ccutils.ProbeClusterInfo, bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.state != ProbeControllerStateHangover {
		return ccutils.ProbeClusterInfoInvalid, false
	}

	settleWait := time.Duration(float64(p.params.Config.SettleWaitNumRTT) * p.rtt * float64(time.Second))
	if settleWait < p.params.Config.SettleWaitMin {
		settleWait = p.params.Config.SettleWaitMin
	}
	if settleWait > p.params.Config.SettleWaitMax {
		settleWait = p.params.Config.SettleWaitMax
	}
	if time.Since(p.stateSwitchedAt) < settleWait {
		return ccutils.ProbeClusterInfoInvalid, false
	}

	p.setState(ProbeControllerStateNone)
	return p.pci, true
}

func (p *ProbeController) ProbeCongestionSignal(isCongestionClearing bool) {
	if !isCongestionClearing {
		// wait longer till next probe
		p.probeInterval = time.Duration(p.probeInterval.Seconds()*p.params.Config.BackoffFactor) * time.Second
		if p.probeInterval > p.params.Config.MaxInterval {
			p.probeInterval = p.params.Config.MaxInterval
		}

		// revert back to starting with shortest probe
		p.probeDuration = p.params.Config.MinDuration
	} else {
		// probe can be started again after minimal interval as previous congestion signal indicated congestion clearing
		p.probeInterval = p.params.Config.BaseInterval

		// can do longer probe after a good probe
		p.probeDuration = time.Duration(float64(p.probeDuration.Milliseconds())*p.params.Config.DurationIncreaseFactor) * time.Millisecond
		if p.probeDuration > p.params.Config.MaxDuration {
			p.probeDuration = p.params.Config.MaxDuration
		}
	}

	if p.pci.CreatedAt.IsZero() {
		p.nextProbeEarliestAt = mono.Now().Add(p.probeInterval)
	} else {
		p.nextProbeEarliestAt = p.pci.CreatedAt.Add(p.probeInterval)
	}
}

func (p *ProbeController) GetActiveProbeClusterId() ccutils.ProbeClusterId {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if p.state == ProbeControllerStateNone {
		return ccutils.ProbeClusterIdInvalid
	}

	return p.pci.Id
}

func (p *ProbeController) setState(state ProbeControllerState) {
	if state == p.state {
		return
	}

	p.state = state
	p.stateSwitchedAt = mono.Now()
}

// ------------------------------------------------
