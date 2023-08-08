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
	"time"

	"github.com/livekit/protocol/logger"
)

// ------------------------------------------------

type TrendDirection int

const (
	TrendDirectionNeutral TrendDirection = iota
	TrendDirectionUpward
	TrendDirectionDownward
)

func (t TrendDirection) String() string {
	switch t {
	case TrendDirectionNeutral:
		return "NEUTRAL"
	case TrendDirectionUpward:
		return "UPWARD"
	case TrendDirectionDownward:
		return "DOWNWARD"
	default:
		return fmt.Sprintf("%d", int(t))
	}
}

// ------------------------------------------------

type trendDetectorSample struct {
	value int64
	at    time.Time
}

func trendDetectorSampleListToString(samples []trendDetectorSample) string {
	samplesStr := ""
	if len(samples) > 0 {
		firstTime := samples[0].at
		samplesStr += "["
		for i, sample := range samples {
			suffix := ", "
			if i == len(samples)-1 {
				suffix = ""
			}
			samplesStr += fmt.Sprintf("%d(%d)%s", sample.value, sample.at.Sub(firstTime).Milliseconds(), suffix)
		}
		samplesStr += "]"
	}
	return samplesStr
}

// ------------------------------------------------

type TrendDetectorParams struct {
	Name                   string
	Logger                 logger.Logger
	RequiredSamples        int
	RequiredSamplesMin     int
	DownwardTrendThreshold float64
	DownwardTrendMaxWait   time.Duration
	ValidityWindow         time.Duration
}

type TrendDetector struct {
	params TrendDetectorParams

	startTime    time.Time
	numSamples   int
	samples      []trendDetectorSample
	lowestValue  int64
	highestValue int64

	direction TrendDirection
}

func NewTrendDetector(params TrendDetectorParams) *TrendDetector {
	return &TrendDetector{
		params:    params,
		startTime: time.Now(),
		direction: TrendDirectionNeutral,
	}
}

func (t *TrendDetector) Seed(value int64) {
	if len(t.samples) != 0 {
		return
	}

	t.samples = append(t.samples, trendDetectorSample{value: value, at: time.Now()})
}

func (t *TrendDetector) AddValue(value int64) {
	t.numSamples++
	if t.lowestValue == 0 || value < t.lowestValue {
		t.lowestValue = value
	}
	if value > t.highestValue {
		t.highestValue = value
	}

	// Collapse successive same value.
	//
	// Bandwidth estimate is received periodically. If the estimate does not change, it will be repeated.
	// When there is congestion, there are several estimates received with decreasing values.
	//
	// Using a sliding window, collapsing repeated values and waiting for falling trend is to ensure that
	// the reaction is not too fast, i. e. reacting to falling values too quick could mean a lot of re-allocation
	// resulting in layer switches, key frames and more congestion.
	//
	// On the flip side, estimate could fall once or twice within a sliding window and stay there.
	// In those cases also, relying on multiple different value readings in the validity window.
	// Those will trend down when there is congestion. If the value drops once and stays there
	// for the entirety of the validity window, it will not be declared as congestion.
	// This scheme is really relying on a series of falling values in a recent time window to declare congestion.
	var lastSample *trendDetectorSample
	if len(t.samples) != 0 {
		lastSample = &t.samples[len(t.samples)-1]
	}
	if lastSample != nil && lastSample.value == value {
		// update time of repeated value
		lastSample.at = time.Now()
	} else {
		t.samples = append(t.samples, trendDetectorSample{value: value, at: time.Now()})
	}
	t.prune()
	t.updateDirection()
}

func (t *TrendDetector) GetLowest() int64 {
	return t.lowestValue
}

func (t *TrendDetector) GetHighest() int64 {
	return t.highestValue
}

func (t *TrendDetector) GetDirection() TrendDirection {
	return t.direction
}

func (t *TrendDetector) HasEnoughSamples() bool {
	return t.numSamples >= t.params.RequiredSamples
}

func (t *TrendDetector) ToString() string {
	now := time.Now()
	elapsed := now.Sub(t.startTime).Seconds()
	return fmt.Sprintf("n: %s, t: %+v|%+v|%.2fs, v: %d|%d|%d|%s|%.2f",
		t.params.Name,
		t.startTime.Format(time.UnixDate), now.Format(time.UnixDate), elapsed,
		t.numSamples, t.lowestValue, t.highestValue, trendDetectorSampleListToString(t.samples), kendallsTau(t.samples),
	)
}

func (t *TrendDetector) prune() {
	// prune based on a few rules

	//  1. If there are more than required samples
	if len(t.samples) > t.params.RequiredSamples {
		t.samples = t.samples[len(t.samples)-t.params.RequiredSamples:]
	}

	// 2. drop samples that are too old
	if len(t.samples) != 0 && t.params.ValidityWindow > 0 {
		cutoffTime := time.Now().Add(-t.params.ValidityWindow)
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
}

func (t *TrendDetector) updateDirection() {
	if len(t.samples) < t.params.RequiredSamplesMin {
		t.direction = TrendDirectionNeutral
		return
	}

	// using Kendall's Tau to find trend
	kt := kendallsTau(t.samples)

	t.direction = TrendDirectionNeutral
	switch {
	case kt > 0 && len(t.samples) >= t.params.RequiredSamples:
		t.direction = TrendDirectionUpward
	case kt < t.params.DownwardTrendThreshold && (len(t.samples) >= t.params.RequiredSamples || t.samples[len(t.samples)-1].at.Sub(t.samples[0].at) > t.params.DownwardTrendMaxWait):
		t.direction = TrendDirectionDownward
	}
}

// ------------------------------------------------

func kendallsTau(samples []trendDetectorSample) float64 {
	concordantPairs := 0
	discordantPairs := 0

	for i := 0; i < len(samples)-1; i++ {
		for j := i + 1; j < len(samples); j++ {
			if samples[i].value < samples[j].value {
				concordantPairs++
			} else if samples[i].value > samples[j].value {
				discordantPairs++
			}
		}
	}

	if (concordantPairs + discordantPairs) == 0 {
		return 0.0
	}

	return (float64(concordantPairs) - float64(discordantPairs)) / (float64(concordantPairs) + float64(discordantPairs))
}
