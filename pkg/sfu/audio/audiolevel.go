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

package audio

import (
	"math"
	"sync"
)

const (
	silentAudioLevel = 127
	negInv20         = -1.0 / 20
)

// --------------------------------------

type AudioLevelConfig struct {
	// minimum level to be considered active, 0-127, where 0 is loudest
	ActiveLevel uint8 `yaml:"active_level,omitempty"`
	// percentile to measure, a participant is considered active if it has exceeded the ActiveLevel more than
	// MinPercentile% of the time
	MinPercentile uint8 `yaml:"min_percentile,omitempty"`
	// interval to update clients, in ms
	UpdateInterval uint32 `yaml:"update_interval,omitempty"`
	// smoothing for audioLevel values sent to the client.
	// audioLevel will be an average of `smooth_intervals`, 0 to disable
	SmoothIntervals uint32 `yaml:"smooth_intervals,omitempty"`
}

var (
	DefaultAudioLevelConfig = AudioLevelConfig{
		ActiveLevel:     35, // -35dBov
		MinPercentile:   40,
		UpdateInterval:  400,
		SmoothIntervals: 2,
	}
)

// --------------------------------------

type AudioLevelParams struct {
	Config AudioLevelConfig
}

// keeps track of audio level for a participant
type AudioLevel struct {
	params AudioLevelParams
	// min duration within an observe duration window to be considered active
	minActiveDuration uint32
	smoothFactor      float64
	activeThreshold   float64

	lock          sync.Mutex
	smoothedLevel float64

	loudestObservedLevel uint8
	activeDuration       uint32 // ms
	observedDuration     uint32 // ms
	lastObservedAt       int64
}

func NewAudioLevel(params AudioLevelParams) *AudioLevel {
	l := &AudioLevel{
		params:               params,
		minActiveDuration:    uint32(params.Config.MinPercentile) * params.Config.UpdateInterval / 100,
		smoothFactor:         1,
		activeThreshold:      ConvertAudioLevel(float64(params.Config.ActiveLevel)),
		loudestObservedLevel: silentAudioLevel,
	}

	if l.params.Config.SmoothIntervals > 0 {
		// exponential moving average (EMA), same center of mass with simple moving average (SMA)
		l.smoothFactor = float64(2) / (float64(l.params.Config.SmoothIntervals + 1))
	}

	return l
}

// Observes a new frame
func (l *AudioLevel) Observe(level uint8, durationMs uint32, arrivalTime int64) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.lastObservedAt = arrivalTime

	l.observedDuration += durationMs

	if level <= l.params.Config.ActiveLevel {
		l.activeDuration += durationMs
		if l.loudestObservedLevel > level {
			l.loudestObservedLevel = level
		}
	}

	if l.observedDuration >= l.params.Config.UpdateInterval {
		smoothedLevel := float64(0.0)
		// compute and reset
		if l.activeDuration >= l.minActiveDuration {
			// adjust loudest observed level by how much of the window was active.
			// Weight will be 0 if active the entire duration
			// > 0 if active for longer than observe duration
			// < 0 if active for less than observe duration
			activityWeight := 20 * math.Log10(float64(l.activeDuration)/float64(l.params.Config.UpdateInterval))
			adjustedLevel := float64(l.loudestObservedLevel) - activityWeight
			linearLevel := ConvertAudioLevel(adjustedLevel)

			// exponential smoothing to dampen transients
			smoothedLevel = l.smoothedLevel + (linearLevel-l.smoothedLevel)*l.smoothFactor
		}
		l.resetLocked(smoothedLevel)
	}
}

// returns current smoothed audio level
func (l *AudioLevel) GetLevel(now int64) (float64, bool) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.resetIfStaleLocked(now)

	return l.smoothedLevel, l.smoothedLevel >= l.activeThreshold
}

func (l *AudioLevel) resetIfStaleLocked(arrivalTime int64) {
	if (arrivalTime-l.lastObservedAt)/1e6 < int64(2*l.params.Config.UpdateInterval) {
		return
	}

	l.resetLocked(0.0)
}

func (l *AudioLevel) resetLocked(smoothedLevel float64) {
	l.smoothedLevel = smoothedLevel
	l.loudestObservedLevel = silentAudioLevel
	l.activeDuration = 0
	l.observedDuration = 0
}

// ---------------------------------------------------

// convert decibel back to linear
func ConvertAudioLevel(level float64) float64 {
	return math.Pow(10, level*negInv20)
}
