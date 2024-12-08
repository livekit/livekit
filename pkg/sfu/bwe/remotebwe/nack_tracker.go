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
	"time"

	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/logger"
)

// ------------------------------------------------

type NackTrackerConfig struct {
	WindowMinDuration time.Duration `yaml:"window_min_duration,omitempty"`
	WindowMaxDuration time.Duration `yaml:"window_max_duration,omitempty"`
	RatioThreshold    float64       `yaml:"ratio_threshold,omitempty"`
}

var (
	defaultNackTrackerConfigProbe = NackTrackerConfig{
		WindowMinDuration: 500 * time.Millisecond,
		WindowMaxDuration: 1 * time.Second,
		RatioThreshold:    0.04,
	}

	defaultNackTrackerConfigNonProbe = NackTrackerConfig{
		WindowMinDuration: 2 * time.Second,
		WindowMaxDuration: 3 * time.Second,
		RatioThreshold:    0.08,
	}
)

// ------------------------------------------------

type nackTrackerParams struct {
	Name   string
	Logger logger.Logger
	Config NackTrackerConfig
}

type nackTracker struct {
	params nackTrackerParams

	windowStartTime time.Time
	packets         uint32
	repeatedNacks   uint32
}

func newNackTracker(params nackTrackerParams) *nackTracker {
	return &nackTracker{
		params: params,
	}
}

func (n *nackTracker) Add(packets uint32, repeatedNacks uint32) {
	if n.params.Config.WindowMaxDuration != 0 && !n.windowStartTime.IsZero() && time.Since(n.windowStartTime) > n.params.Config.WindowMaxDuration {
		n.windowStartTime = time.Time{}
		n.packets = 0
		n.repeatedNacks = 0
	}

	//
	// Start NACK monitoring window only when a repeated NACK happens.
	// This allows locking tightly to when NACKs start happening and
	// check if the NACKs keep adding up (potentially a sign of congestion)
	// or isolated losses
	//
	if n.repeatedNacks == 0 && repeatedNacks != 0 {
		n.windowStartTime = time.Now()
	}

	if !n.windowStartTime.IsZero() {
		n.packets += packets
		n.repeatedNacks += repeatedNacks
	}
}

func (n *nackTracker) GetRatio() float64 {
	ratio := 0.0
	if n.packets != 0 {
		ratio = float64(n.repeatedNacks) / float64(n.packets)
		if ratio > 1.0 {
			ratio = 1.0
		}
	}

	return ratio
}

func (n *nackTracker) IsTriggered() bool {
	if n.params.Config.WindowMinDuration != 0 && !n.windowStartTime.IsZero() && time.Since(n.windowStartTime) > n.params.Config.WindowMinDuration {
		return n.GetRatio() > n.params.Config.RatioThreshold
	}

	return false
}

func (n *nackTracker) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if n == nil {
		return nil
	}

	e.AddString("name", n.params.Name)
	if n.windowStartTime.IsZero() {
		e.AddString("window", "inactive")
	} else {
		e.AddTime("windowStartTime", n.windowStartTime)
		e.AddDuration("windowDuration", time.Since(n.windowStartTime))
		e.AddUint32("packets", n.packets)
		e.AddUint32("repeatedNacks", n.repeatedNacks)
		e.AddFloat64("nackRatio", n.GetRatio())
	}
	return nil
}

// ------------------------------------------------
