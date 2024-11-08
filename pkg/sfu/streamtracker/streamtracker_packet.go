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
	"time"

	"github.com/livekit/protocol/logger"
)

// --------------------------------------------

type StreamTrackerPacketConfig struct {
	SamplesRequired uint32        `yaml:"samples_required,omitempty"` // number of samples needed per cycle
	CyclesRequired  uint32        `yaml:"cycles_required,omitempty"`  // number of cycles needed to be active
	CycleDuration   time.Duration `yaml:"cycle_duration,omitempty"`
}

var (
	DefaultStreamTrackerPacketConfigVideo = map[int32]StreamTrackerPacketConfig{
		0: {SamplesRequired: 1,
			CyclesRequired: 4,
			CycleDuration:  500 * time.Millisecond,
		},
		1: {SamplesRequired: 5,
			CyclesRequired: 20,
			CycleDuration:  500 * time.Millisecond,
		},
		2: {SamplesRequired: 5,
			CyclesRequired: 20,
			CycleDuration:  500 * time.Millisecond,
		},
	}

	DefaultStreamTrackerPacketConfigScreenshare = map[int32]StreamTrackerPacketConfig{
		0: {
			SamplesRequired: 1,
			CyclesRequired:  1,
			CycleDuration:   2 * time.Second,
		},
		1: {
			SamplesRequired: 1,
			CyclesRequired:  1,
			CycleDuration:   2 * time.Second,
		},
		2: {
			SamplesRequired: 1,
			CyclesRequired:  1,
			CycleDuration:   2 * time.Second,
		},
	}
)

// --------------------------------------------

type StreamTrackerPacketParams struct {
	Config StreamTrackerPacketConfig
	Logger logger.Logger
}

type StreamTrackerPacket struct {
	params StreamTrackerPacketParams

	countSinceLast uint32 // number of packets received since last check

	initialized bool

	cycleCount uint32
}

func NewStreamTrackerPacket(params StreamTrackerPacketParams) StreamTrackerImpl {
	return &StreamTrackerPacket{
		params: params,
	}
}

func (s *StreamTrackerPacket) Start() {
}

func (s *StreamTrackerPacket) Stop() {
}

func (s *StreamTrackerPacket) Reset() {
	s.countSinceLast = 0
	s.cycleCount = 0

	s.initialized = false
}

func (s *StreamTrackerPacket) GetCheckInterval() time.Duration {
	return s.params.Config.CycleDuration
}

func (s *StreamTrackerPacket) Observe(_hasMarker bool, _ts uint32) StreamStatusChange {
	if !s.initialized {
		// first packet
		s.initialized = true
		s.countSinceLast = 1
		return StreamStatusChangeActive
	}

	s.countSinceLast++
	return StreamStatusChangeNone
}

func (s *StreamTrackerPacket) CheckStatus() StreamStatusChange {
	if !s.initialized {
		// should not be getting called when not initialized, but be safe
		return StreamStatusChangeNone
	}

	if s.countSinceLast >= s.params.Config.SamplesRequired {
		s.cycleCount++
	} else {
		s.cycleCount = 0
	}

	statusChange := StreamStatusChangeNone
	if s.cycleCount == 0 {
		// no packets seen for a period, flip to stopped
		statusChange = StreamStatusChangeStopped
	} else if s.cycleCount >= s.params.Config.CyclesRequired {
		// packets seen for some time after resume, flip to active
		statusChange = StreamStatusChangeActive
	}

	s.countSinceLast = 0
	return statusChange
}
