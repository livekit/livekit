package streamtracker

import (
	"time"

	"github.com/livekit/protocol/logger"
)

type StreamTrackerPacketParams struct {
	// number of samples needed per cycle
	SamplesRequired uint32

	// number of cycles needed to be active
	CyclesRequired uint32

	CycleDuration time.Duration

	Logger logger.Logger
}

// StreamTracker keeps track of packet flow and ensures a particular up track is consistently producing
// It runs its own goroutine for detection, and fires OnStatusChanged callback
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
	return s.params.CycleDuration
}

// Observe a packet that's received
func (s *StreamTrackerPacket) Observe(temporalLayer int32, pktSize int, payloadSize int) bool {
	if !s.initialized {
		// first packet
		s.initialized = true
		s.countSinceLast = 1
		return true
	}

	s.countSinceLast++
	return false
}

func (s *StreamTrackerPacket) CheckStatus() StreamStatus {
	if s.countSinceLast >= s.params.SamplesRequired {
		s.cycleCount++
	} else {
		s.cycleCount = 0
	}

	var status StreamStatus
	if s.cycleCount == 0 {
		// no packets seen for a period, flip to stopped
		status = StreamStatusStopped
	} else if s.cycleCount >= s.params.CyclesRequired {
		// packets seen for some time after resume, flip to active
		status = StreamStatusActive
	}

	s.countSinceLast = 0
	return status
}
