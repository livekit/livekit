package buffer

import (
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/protocol/livekit"
)

type DataStatsParam struct {
	WindowDuration time.Duration
}

type DataStats struct {
	params      DataStatsParam
	lock        sync.RWMutex
	totalBytes  int64
	startTime   time.Time
	endTime     time.Time
	windowStart int64
	windowBytes int64
}

func NewDataStats(params DataStatsParam) *DataStats {
	return &DataStats{
		params:      params,
		startTime:   time.Now(),
		windowStart: time.Now().UnixNano(),
	}
}

func (s *DataStats) Update(bytes int, time int64) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.totalBytes += int64(bytes)

	if s.params.WindowDuration > 0 && time-s.windowStart > s.params.WindowDuration.Nanoseconds() {
		s.windowBytes = 0
		s.windowStart = time
	}
	s.windowBytes += int64(bytes)
}

func (s *DataStats) ToProtoActive() *livekit.RTPStats {
	if s.params.WindowDuration == 0 {
		return &livekit.RTPStats{}
	}
	s.lock.RLock()
	defer s.lock.RUnlock()
	now := time.Now().UnixNano()
	duration := now - s.windowStart
	if duration > s.params.WindowDuration.Nanoseconds() {
		return &livekit.RTPStats{}
	}

	return &livekit.RTPStats{
		StartTime: timestamppb.New(time.Unix(s.windowStart/1e9, s.windowStart%1e9)),
		EndTime:   timestamppb.New(time.Now()),
		Duration:  float64(duration / 1e9),
		Bytes:     uint64(s.windowBytes),
		Bitrate:   float64(s.windowBytes) * 8 / float64(duration) / 1e9,
	}
}

func (s *DataStats) Stop() {
	s.lock.Lock()
	s.endTime = time.Now()
	s.lock.Unlock()
}

func (s *DataStats) ToProtoAggregateOnly() *livekit.RTPStats {
	s.lock.RLock()
	defer s.lock.RUnlock()

	end := s.endTime
	if end.IsZero() {
		end = time.Now()
	}
	return &livekit.RTPStats{
		StartTime: timestamppb.New(s.startTime),
		EndTime:   timestamppb.New(end),
		Duration:  end.Sub(s.startTime).Seconds(),
		Bytes:     uint64(s.windowBytes),
		Bitrate:   float64(s.windowBytes) * 8 / end.Sub(s.startTime).Seconds(),
	}
}
