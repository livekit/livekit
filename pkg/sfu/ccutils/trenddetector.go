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
	"fmt"
	"time"

	"github.com/livekit/protocol/logger"
	"go.uber.org/zap/zapcore"
)

// ------------------------------------------------

type TrendDirection int

const (
	TrendDirectionInconclusive TrendDirection = iota
	TrendDirectionUpward
	TrendDirectionDownward
)

func (t TrendDirection) String() string {
	switch t {
	case TrendDirectionInconclusive:
		return "INCONCLUSIVE"
	case TrendDirectionUpward:
		return "UPWARD"
	case TrendDirectionDownward:
		return "DOWNWARD"
	default:
		return fmt.Sprintf("%d", int(t))
	}
}

// ------------------------------------------------

type trendDetectorNumber interface {
	int64 | float64
}

// ------------------------------------------------

type trendDetectorSample[T trendDetectorNumber] struct {
	value T
	at    time.Time
}

type trendDetectorSampleElapsed[T trendDetectorNumber] struct {
	value      T
	sinceFirst time.Duration
}

func (t trendDetectorSampleElapsed[T]) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddFloat64("value", float64(t.value))
	e.AddDuration("sinceFirst", t.sinceFirst)
	return nil
}

// ------------------------------------------------

type TrendDetectorConfig struct {
	RequiredSamples        int           `yaml:"required_samples,omitempty"`
	RequiredSamplesMin     int           `yaml:"required_samples_min,omitempty"`
	DownwardTrendThreshold float64       `yaml:"downward_trend_threshold,omitempty"`
	DownwardTrendMaxWait   time.Duration `yaml:"downward_trend_max_wait,omitempty"`
	CollapseThreshold      time.Duration `yaml:"collapse_threshold,omitempty"`
	ValidityWindow         time.Duration `yaml:"validity_window,omitempty"`
}

// ------------------------------------------------

type TrendDetectorParams struct {
	Name   string
	Logger logger.Logger
	Config TrendDetectorConfig
}

type TrendDetector[T trendDetectorNumber] struct {
	params TrendDetectorParams

	startTime    time.Time
	numSamples   int
	samples      []trendDetectorSample[T]
	lowestValue  T
	highestValue T

	direction TrendDirection
}

func NewTrendDetector[T trendDetectorNumber](params TrendDetectorParams) *TrendDetector[T] {
	return &TrendDetector[T]{
		params:    params,
		startTime: time.Now(),
		direction: TrendDirectionInconclusive,
	}
}

func (t *TrendDetector[T]) Seed(value T) {
	if len(t.samples) != 0 {
		return
	}

	t.samples = append(t.samples, trendDetectorSample[T]{value: value, at: time.Now()})
}

func (t *TrendDetector[T]) AddValue(value T) {
	t.numSamples++
	if t.lowestValue == 0 || value < t.lowestValue {
		t.lowestValue = value
	}
	if value > t.highestValue {
		t.highestValue = value
	}

	// Ignore duplicate values in collapse window.
	//
	// Bandwidth estimate is received periodically. If the estimate does not change, it will be repeated.
	// When there is congestion, there are several estimates received with decreasing values.
	//
	// Using a sliding window, collapsing repeated values and waiting for falling trend to ensure that
	// the reaction is not too fast, i. e. reacting to falling values too quick could mean a lot of re-allocation
	// resulting in layer switches, key frames and more congestion.
	//
	// But, on the flip side, estimate could fall once or twice within a sliding window and stay there.
	// In those cases, using a collapse window to record a value even if it is duplicate. By doing that,
	// a trend could be detected eventually. It will be delayed, but that is fine with slow changing estimates.
	var lastSample *trendDetectorSample[T]
	if len(t.samples) != 0 {
		lastSample = &t.samples[len(t.samples)-1]
	}
	if lastSample != nil && lastSample.value == value && t.params.Config.CollapseThreshold > 0 && time.Since(lastSample.at) < t.params.Config.CollapseThreshold {
		return
	}

	t.samples = append(t.samples, trendDetectorSample[T]{value: value, at: time.Now()})
	t.prune()
	t.updateDirection()
}

func (t *TrendDetector[T]) GetLowest() T {
	return t.lowestValue
}

func (t *TrendDetector[T]) GetHighest() T {
	return t.highestValue
}

func (t *TrendDetector[T]) GetDirection() TrendDirection {
	return t.direction
}

func (t *TrendDetector[T]) HasEnoughSamples() bool {
	return t.numSamples >= t.params.Config.RequiredSamples
}

func (t *TrendDetector[T]) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if t == nil {
		return nil
	}

	var samples []trendDetectorSampleElapsed[T]
	if len(t.samples) > 0 {
		firstTime := t.samples[0].at
		for _, sample := range t.samples {
			samples = append(samples, trendDetectorSampleElapsed[T]{sample.value, sample.at.Sub(firstTime)})
		}
	}

	e.AddString("name", t.params.Name)
	e.AddTime("startTime", t.startTime)
	e.AddDuration("elapsed", time.Since(t.startTime))
	e.AddInt("numSamples", t.numSamples)
	e.AddArray("samples", logger.ObjectSlice(samples))
	e.AddFloat64("lowestValue", float64(t.lowestValue))
	e.AddFloat64("highestValue", float64(t.highestValue))
	e.AddFloat64("kendallsTau", t.kendallsTau())
	e.AddString("direction", t.direction.String())
	return nil
}

func (t *TrendDetector[T]) prune() {
	// prune based on a few rules

	//  1. If there are more than required samples
	if len(t.samples) > t.params.Config.RequiredSamples {
		t.samples = t.samples[len(t.samples)-t.params.Config.RequiredSamples:]
	}

	// 2. drop samples that are too old
	if len(t.samples) != 0 && t.params.Config.ValidityWindow > 0 {
		cutoffTime := time.Now().Add(-t.params.Config.ValidityWindow)
		cutoffIndex := -1
		for i := 0; i < len(t.samples); i++ {
			if t.samples[i].at.After(cutoffTime) {
				cutoffIndex = i
				break
			}
		}
		if cutoffIndex >= 0 {
			t.samples = t.samples[cutoffIndex:]
		}
	}

	//  3. collapse same values at the front to just the last of those samples
	if len(t.samples) != 0 {
		cutoffIndex := -1
		firstValue := t.samples[0].value
		for i := 1; i < len(t.samples); i++ {
			if t.samples[i].value != firstValue {
				cutoffIndex = i - 1
				break
			}
		}

		if cutoffIndex >= 0 {
			t.samples = t.samples[cutoffIndex:]
		} else {
			// all values are the same, just keep the last one
			t.samples = t.samples[len(t.samples)-1:]
		}
	}
}

func (t *TrendDetector[T]) updateDirection() {
	if len(t.samples) < t.params.Config.RequiredSamplesMin {
		t.direction = TrendDirectionInconclusive
		return
	}

	// using Kendall's Tau to find trend
	kt := t.kendallsTau()

	t.direction = TrendDirectionInconclusive
	switch {
	case kt > 0 && len(t.samples) >= t.params.Config.RequiredSamples:
		t.direction = TrendDirectionUpward
	case kt < t.params.Config.DownwardTrendThreshold && (len(t.samples) >= t.params.Config.RequiredSamples || t.samples[len(t.samples)-1].at.Sub(t.samples[0].at) > t.params.Config.DownwardTrendMaxWait):
		t.direction = TrendDirectionDownward
	}
}

func (t *TrendDetector[T]) kendallsTau() float64 {
	concordantPairs := 0
	discordantPairs := 0

	for i := 0; i < len(t.samples)-1; i++ {
		for j := i + 1; j < len(t.samples); j++ {
			if t.samples[i].value < t.samples[j].value {
				concordantPairs++
			} else if t.samples[i].value > t.samples[j].value {
				discordantPairs++
			}
		}
	}

	if (concordantPairs + discordantPairs) == 0 {
		return 0.0
	}

	return (float64(concordantPairs) - float64(discordantPairs)) / (float64(concordantPairs) + float64(discordantPairs))
}
