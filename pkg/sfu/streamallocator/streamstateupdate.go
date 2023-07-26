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
