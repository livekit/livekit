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

package ccutils

import (
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

// ------------------------------------------------

type ProbeRegulatorConfig struct {
	BaseInterval  time.Duration `yaml:"base_interval,omitempty"`
	BackoffFactor float64       `yaml:"backoff_factor,omitempty"`
	MaxInterval   time.Duration `yaml:"max_interval,omitempty"`

	MinDuration            time.Duration `yaml:"min_duration,omitempty"`
	MaxDuration            time.Duration `yaml:"max_duration,omitempty"`
	DurationIncreaseFactor float64       `yaml:"duration_increase_factor,omitempty"`
}

var (
	DefaultProbeRegulatorConfig = ProbeRegulatorConfig{
		BaseInterval:  3 * time.Second,
		BackoffFactor: 1.5,
		MaxInterval:   2 * time.Minute,

		MinDuration:            200 * time.Millisecond,
		MaxDuration:            20 * time.Second,
		DurationIncreaseFactor: 1.5,
	}
)

// ---------------------------------------------------------------------------

type ProbeRegulatorParams struct {
	Config ProbeRegulatorConfig
	Logger logger.Logger
}

type ProbeRegulator struct {
	params ProbeRegulatorParams

	probeInterval       time.Duration
	probeDuration       time.Duration
	nextProbeEarliestAt time.Time
}

func NewProbeRegulator(params ProbeRegulatorParams) *ProbeRegulator {
	return &ProbeRegulator{
		params:              params,
		probeInterval:       params.Config.BaseInterval,
		probeDuration:       params.Config.MinDuration,
		nextProbeEarliestAt: mono.Now(),
	}
}

func (p *ProbeRegulator) CanProbe() bool {
	return mono.Now().After(p.nextProbeEarliestAt)
}

func (p *ProbeRegulator) ProbeDuration() time.Duration {
	return p.probeDuration
}

func (p *ProbeRegulator) ProbeSignal(probeSignal ProbeSignal, baseTime time.Time) {
	if probeSignal == ProbeSignalCongesting {
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

	if baseTime.IsZero() {
		p.nextProbeEarliestAt = mono.Now().Add(p.probeInterval)
	} else {
		p.nextProbeEarliestAt = baseTime.Add(p.probeInterval)
	}
}
