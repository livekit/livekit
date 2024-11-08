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

package streamtracker

import (
	"math"
	"time"

	"github.com/livekit/protocol/logger"
)

const (
	checkInterval           = 500 * time.Millisecond
	statusCheckTolerance    = 0.98
	frameRateResolution     = 0.01 // 1 frame every 100 seconds
	frameRateIncreaseFactor = 0.6  // slow increase
	frameRateDecreaseFactor = 0.9  // fast decrease
)

// -------------------------------------------------------

type StreamTrackerFrameConfig struct {
	MinFPS float64 `yaml:"min_fps,omitempty"`
}

var (
	DefaultStreamTrackerFrameConfigVideo = map[int32]StreamTrackerFrameConfig{
		0: {
			MinFPS: 5.0,
		},
		1: {
			MinFPS: 5.0,
		},
		2: {
			MinFPS: 5.0,
		},
	}

	DefaultStreamTrackerFrameConfigScreenshare = map[int32]StreamTrackerFrameConfig{
		0: {
			MinFPS: 0.5,
		},
		1: {
			MinFPS: 0.5,
		},
		2: {
			MinFPS: 0.5,
		},
	}
)

// -------------------------------------------------------

type StreamTrackerFrameParams struct {
	Config    StreamTrackerFrameConfig
	ClockRate uint32
	Logger    logger.Logger
}

type StreamTrackerFrame struct {
	params StreamTrackerFrameParams

	initialized bool

	tsInitialized bool
	oldestTS      uint32
	newestTS      uint32
	numFrames     int

	estimatedFrameRate float64
	evalInterval       time.Duration
	lastStatusCheckAt  time.Time
}

func NewStreamTrackerFrame(params StreamTrackerFrameParams) StreamTrackerImpl {
	s := &StreamTrackerFrame{
		params: params,
	}
	s.Reset()
	return s
}

func (s *StreamTrackerFrame) Start() {
}

func (s *StreamTrackerFrame) Stop() {
}

func (s *StreamTrackerFrame) Reset() {
	s.initialized = false

	s.resetFPSCalculator()

	s.lastStatusCheckAt = time.Time{}
}

func (s *StreamTrackerFrame) resetFPSCalculator() {
	s.tsInitialized = false
	s.oldestTS = 0
	s.newestTS = 0
	s.numFrames = 0

	s.estimatedFrameRate = 0.0
	s.updateEvalInterval()
}

func (s *StreamTrackerFrame) GetCheckInterval() time.Duration {
	return checkInterval
}

func (s *StreamTrackerFrame) Observe(hasMarker bool, ts uint32) StreamStatusChange {
	if hasMarker {
		if !s.tsInitialized {
			s.tsInitialized = true
			s.oldestTS = ts
			s.newestTS = ts
			s.numFrames = 1
		} else {
			diff := ts - s.oldestTS
			if diff > (1 << 31) {
				s.oldestTS = ts
			}
			diff = ts - s.newestTS
			if diff < (1 << 31) {
				s.newestTS = ts
			}
			s.numFrames++
		}
	}

	// When starting up, check for first packet and declare active.
	// Happens under following conditions
	//   1. Start up
	//   2. Unmute (stream restarting)
	//   3. Layer starting after dynacast pause
	if !s.initialized {
		s.initialized = true
		s.lastStatusCheckAt = time.Now()
		return StreamStatusChangeActive
	}

	return StreamStatusChangeNone
}

func (s *StreamTrackerFrame) CheckStatus() StreamStatusChange {
	if !s.initialized {
		// should not be getting called when not initialized, but be safe
		return StreamStatusChangeNone
	}

	if !s.updateStatusCheckTime() {
		return StreamStatusChangeNone
	}

	if s.updateEstimatedFrameRate() == 0.0 {
		// when stream is stopped, reset FPS calculator to ensure re-start is not done until at least two frames are available,
		// i. e. enough frames available to be able to calculate FPS
		s.resetFPSCalculator()
		return StreamStatusChangeStopped
	}

	return StreamStatusChangeActive
}

func (s *StreamTrackerFrame) updateStatusCheckTime() bool {
	// check only at intervals based on estimated frame rate
	if s.lastStatusCheckAt.IsZero() {
		s.lastStatusCheckAt = time.Now()
	}
	if time.Since(s.lastStatusCheckAt) < time.Duration(statusCheckTolerance*float64(s.evalInterval)) {
		return false
	}
	s.lastStatusCheckAt = time.Now()
	return true
}

func (s *StreamTrackerFrame) updateEstimatedFrameRate() float64 {
	diff := s.newestTS - s.oldestTS
	if diff == 0 || s.numFrames < 2 {
		return 0.0
	}

	frameRate := roundFrameRate(float64(s.params.ClockRate) / float64(diff) * float64(s.numFrames-1))

	// reset for next evaluation interval
	s.oldestTS = s.newestTS
	s.numFrames = 1

	factor := 1.0
	switch {
	case s.estimatedFrameRate < frameRate:
		// slow increase, prevents shortening eval interval too quickly on frame rate going up
		factor = frameRateIncreaseFactor
	case s.estimatedFrameRate > frameRate:
		// fast decrease, prevents declaring stream stop too quickly on frame rate going down
		factor = frameRateDecreaseFactor
	}

	estimatedFrameRate := roundFrameRate(frameRate*factor + s.estimatedFrameRate*(1.0-factor))
	if s.estimatedFrameRate != estimatedFrameRate {
		s.estimatedFrameRate = estimatedFrameRate
		s.updateEvalInterval()
		s.params.Logger.Debugw("updating estimated frame rate", "estimatedFPS", estimatedFrameRate, "evalInterval", s.evalInterval)
	}

	return frameRate
}

func (s *StreamTrackerFrame) updateEvalInterval() {
	// STREAM-TRACKER-FRAME-TODO: This will run into challenges for frame rate falling steeply, How to address that?
	// Maybe, look at some referential rules (between layers) for possibilities to solve it. Currently, this is addressed
	// by setting a source aware min FPS to ensure evaluation window is long enough to avoid declaring stop too quickly.
	s.evalInterval = checkInterval
	if s.estimatedFrameRate > 0.0 {
		estimatedFrameRateInterval := time.Duration(float64(time.Second) / s.estimatedFrameRate)
		if estimatedFrameRateInterval > s.evalInterval {
			s.evalInterval = estimatedFrameRateInterval
		}
	}
	if s.params.Config.MinFPS > 0.0 {
		minFPSInterval := time.Duration(float64(time.Second) / s.params.Config.MinFPS)
		if minFPSInterval > s.evalInterval {
			s.evalInterval = minFPSInterval
		}
	}
}

// ------------------------------------------------------------------------------

func roundFrameRate(frameRate float64) float64 {
	return math.Round(frameRate/frameRateResolution) * frameRateResolution
}
