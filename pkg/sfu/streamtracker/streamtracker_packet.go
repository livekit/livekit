package streamtracker

import (
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/logger"
)

type StreamTrackerPacketParams struct {
	Config config.StreamTrackerPacketConfig
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
