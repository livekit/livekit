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
	ProbeRegulator ccutils.ProbeRegulatorConfig `yaml:"probe_regulator,omitempty"`

	SettleWaitNumRTT uint32        `yaml:"settle_wait_num_rtt,omitempty"`
	SettleWaitMin    time.Duration `yaml:"settle_wait_min,omitempty"`
	SettleWaitMax    time.Duration `yaml:"settle_wait_max,omitempty"`
}

var (
	DefaultProbeControllerConfig = ProbeControllerConfig{
		ProbeRegulator: ccutils.DefaultProbeRegulatorConfig,

		SettleWaitNumRTT: 5,
		SettleWaitMin:    250 * time.Millisecond,
		SettleWaitMax:    5 * time.Second,
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

	*ccutils.ProbeRegulator
}

func newProbeController(params probeControllerParams) *probeController {
	return &probeController{
		params:          params,
		state:           probeControllerStateNone,
		stateSwitchedAt: mono.Now(),
		pci:             ccutils.ProbeClusterInfoInvalid,
		rtt:             bwe.DefaultRTT,
		ProbeRegulator: ccutils.NewProbeRegulator(
			ccutils.ProbeRegulatorParams{
				Config: params.Config.ProbeRegulator,
				Logger: params.Logger,
			},
		),
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

func (p *probeController) GetRTT() float64 {
	return p.rtt
}

func (p *probeController) CanProbe() bool {
	return p.state == probeControllerStateNone && p.ProbeRegulator.CanProbe()
}

func (p *probeController) IsInProbe() bool {
	return p.state != probeControllerStateNone
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

func (p *probeController) ProbeClusterIsGoalReached(estimate int64) bool {
	if p.pci.Id == ccutils.ProbeClusterIdInvalid {
		return false
	}

	return estimate > int64(p.pci.Goal.DesiredBps)
}

func (p *probeController) MaybeFinalizeProbe() (ccutils.ProbeClusterInfo, bool) {
	if p.state != probeControllerStateHangover {
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

	p.setState(probeControllerStateNone)
	return p.pci, true
}

func (p *probeController) setState(state probeControllerState) {
	if state == p.state {
		return
	}

	p.state = state
	p.stateSwitchedAt = mono.Now()
}

// ------------------------------------------------
