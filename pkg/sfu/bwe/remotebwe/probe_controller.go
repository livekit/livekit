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

package remotebwe

import (
	"fmt"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

// ---------------------------------------------------------------------------

type probeControllerState int

const (
	probeControllerStateNone probeControllerState = iota
	probeControllerStateProbing
	probeControllerStateHangover
)

func (p probeControllerState) String() string {
	switch p {
	case probeControllerStateNone:
		return "NONE"
	case probeControllerStateProbing:
		return "PROBING"
	case probeControllerStateHangover:
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

		MinDuration:            200 * time.Millisecond,
		MaxDuration:            20 * time.Second,
		DurationIncreaseFactor: 1.5,
	}
)

// ---------------------------------------------------------------------------

type probeControllerParams struct {
	Config ProbeControllerConfig
	Logger logger.Logger
}

type probeController struct {
	params probeControllerParams

	state           probeControllerState
	stateSwitchedAt time.Time

	pci ccutils.ProbeClusterInfo
	rtt float64

	probeInterval       time.Duration
	probeDuration       time.Duration
	nextProbeEarliestAt time.Time
}

func newProbeController(params probeControllerParams) *probeController {
	return &probeController{
		params:              params,
		state:               probeControllerStateNone,
		stateSwitchedAt:     mono.Now(),
		pci:                 ccutils.ProbeClusterInfoInvalid,
		rtt:                 bwe.DefaultRTT,
		probeInterval:       params.Config.BaseInterval,
		probeDuration:       params.Config.MinDuration,
		nextProbeEarliestAt: mono.Now(),
	}
}

func (p *probeController) UpdateRTT(rtt float64) {
	if rtt == 0 {
		p.rtt = bwe.DefaultRTT
	} else {
		if p.rtt == 0 {
			p.rtt = rtt
		} else {
			p.rtt = bwe.RTTSmoothingFactor*rtt + (1.0-bwe.RTTSmoothingFactor)*p.rtt
		}
	}
}

func (p *probeController) CanProbe() bool {
	return p.state == probeControllerStateNone && mono.Now().After(p.nextProbeEarliestAt)
}

func (p *probeController) IsInProbe() bool {
	return p.state != probeControllerStateNone
}

func (p *probeController) ProbeDuration() time.Duration {
	return p.probeDuration
}

func (p *probeController) ProbeClusterStarting(pci ccutils.ProbeClusterInfo) {
	if p.state != probeControllerStateNone {
		p.params.Logger.Warnw("unexpected probe controller state", nil, "state", p.state)
	}

	p.setState(probeControllerStateProbing)
	p.pci = pci
}

func (p *probeController) ProbeClusterDone(pci ccutils.ProbeClusterInfo) {
	if p.pci.Id != pci.Id {
		return
	}

	p.pci.Result = pci.Result
	p.setState(probeControllerStateHangover)
}

func (p *probeController) MaybeFinalizeProbe() (ccutils.ProbeClusterInfo, bool, error) {
	if p.state != probeControllerStateHangover {
		return ccutils.ProbeClusterInfoInvalid, false, bwe.ErrProbeClusterStateMismatch
	}

	settleWait := time.Duration(float64(p.params.Config.SettleWaitNumRTT) * p.rtt * float64(time.Second))
	if settleWait < p.params.Config.SettleWaitMin {
		settleWait = p.params.Config.SettleWaitMin
	}
	if settleWait > p.params.Config.SettleWaitMax {
		settleWait = p.params.Config.SettleWaitMax
	}
	if time.Since(p.stateSwitchedAt) < settleWait {
		return ccutils.ProbeClusterInfoInvalid, false, nil
	}

	p.setState(probeControllerStateNone)
	return p.pci, true, nil
}

func (p *probeController) ProbeSignal(probeSignal bwe.ProbeSignal) {
	if probeSignal == bwe.ProbeSignalCongesting {
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

func (p *probeController) setState(state probeControllerState) {
	if state == p.state {
		return
	}

	p.state = state
	p.stateSwitchedAt = mono.Now()
}

// ------------------------------------------------
