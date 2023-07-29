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

	"github.com/livekit/protocol/livekit"
)

// ------------------------------------------------

type StreamState int

const (
	StreamStateInactive StreamState = iota
	StreamStateActive
	StreamStatePaused
)

func (s StreamState) String() string {
	switch s {
	case StreamStateInactive:
		return "INACTIVE"
	case StreamStateActive:
		return "ACTIVE"
	case StreamStatePaused:
		return "PAUSED"
	default:
		return fmt.Sprintf("UNKNOWN: %d", int(s))
	}
}

// ------------------------------------------------

type StreamStateInfo struct {
	ParticipantID livekit.ParticipantID
	TrackID       livekit.TrackID
	State         StreamState
}

type StreamStateUpdate struct {
	StreamStates []*StreamStateInfo
}

func NewStreamStateUpdate() *StreamStateUpdate {
	return &StreamStateUpdate{}
}

func (s *StreamStateUpdate) HandleStreamingChange(track *Track, streamState StreamState) {
	switch streamState {
	case StreamStateInactive:
		// inactive is not a notification, could get into this state because of mute
	case StreamStateActive:
		s.StreamStates = append(s.StreamStates, &StreamStateInfo{
			ParticipantID: track.PublisherID(),
			TrackID:       track.ID(),
			State:         StreamStateActive,
		})
	case StreamStatePaused:
		s.StreamStates = append(s.StreamStates, &StreamStateInfo{
			ParticipantID: track.PublisherID(),
			TrackID:       track.ID(),
			State:         StreamStatePaused,
		})
	}
}

func (s *StreamStateUpdate) Empty() bool {
	return len(s.StreamStates) == 0
}

// ------------------------------------------------
