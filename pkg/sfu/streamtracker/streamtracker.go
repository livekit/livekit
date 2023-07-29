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
	"fmt"
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
)

// ------------------------------------------------------------

type StreamStatus int32

func (s StreamStatus) String() string {
	switch s {
	case StreamStatusStopped:
		return "stopped"
	case StreamStatusActive:
		return "active"
	default:
		return fmt.Sprintf("unknown: %d", int(s))
	}
}

const (
	StreamStatusStopped StreamStatus = iota
	StreamStatusActive
)

// ------------------------------------------------------------

type StreamTrackerParams struct {
	StreamTrackerImpl     StreamTrackerImpl
	BitrateReportInterval time.Duration

	Logger logger.Logger
}

type StreamTracker struct {
	params StreamTrackerParams

	onStatusChanged    func(status StreamStatus)
	onBitrateAvailable func()

	lock sync.RWMutex

	paused     bool
	generation atomic.Uint32

	status             StreamStatus
	lastNotifiedStatus StreamStatus

	lastBitrateReport time.Time
	bytesForBitrate   [4]int64
	bitrate           [4]int64

	isStopped bool
}

func NewStreamTracker(params StreamTrackerParams) *StreamTracker {
	return &StreamTracker{
		params: params,
		status: StreamStatusStopped,
	}
}

func (s *StreamTracker) OnStatusChanged(f func(status StreamStatus)) {
	s.onStatusChanged = f
}

func (s *StreamTracker) OnBitrateAvailable(f func()) {
	s.onBitrateAvailable = f
}

func (s *StreamTracker) Status() StreamStatus {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.status
}

func (s *StreamTracker) setStatusLocked(status StreamStatus) {
	s.status = status
}

func (s *StreamTracker) maybeNotifyStatus() {
	var status StreamStatus
	notify := false
	s.lock.Lock()
	if s.status != s.lastNotifiedStatus {
		notify = true
		status = s.status
		s.lastNotifiedStatus = s.status
	}
	s.lock.Unlock()

	if notify && s.onStatusChanged != nil {
		s.onStatusChanged(status)
	}
}

func (s *StreamTracker) Start() {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.params.StreamTrackerImpl.Start()
}

func (s *StreamTracker) Stop() {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.isStopped {
		return
	}
	s.isStopped = true

	// bump generation to trigger exit of worker
	s.generation.Inc()

	s.params.StreamTrackerImpl.Stop()
}

func (s *StreamTracker) Reset() {
	s.lock.Lock()
	if s.isStopped {
		s.lock.Unlock()
		return
	}

	s.resetLocked()
	s.lock.Unlock()

	s.maybeNotifyStatus()
}

func (s *StreamTracker) resetLocked() {
	// bump generation to trigger exit of current worker
	s.generation.Inc()

	s.setStatusLocked(StreamStatusStopped)

	for i := 0; i < len(s.bytesForBitrate); i++ {
		s.bytesForBitrate[i] = 0
	}
	for i := 0; i < len(s.bitrate); i++ {
		s.bitrate[i] = 0
	}

	s.params.StreamTrackerImpl.Reset()
}

func (s *StreamTracker) SetPaused(paused bool) {
	s.lock.Lock()
	s.paused = paused
	if !paused {
		s.resetLocked()
	} else {
		// bump generation to trigger exit of current worker
		s.generation.Inc()

		s.setStatusLocked(StreamStatusStopped)
	}
	s.lock.Unlock()

	s.maybeNotifyStatus()
}

func (s *StreamTracker) Observe(
	temporalLayer int32,
	pktSize int,
	payloadSize int,
	hasMarker bool,
	ts uint32,
	_ *buffer.ExtDependencyDescriptor,
) {
	s.lock.Lock()

	if s.isStopped || s.paused || payloadSize == 0 {
		s.lock.Unlock()
		return
	}

	statusChange := s.params.StreamTrackerImpl.Observe(hasMarker, ts)
	if statusChange == StreamStatusChangeActive {
		s.setStatusLocked(StreamStatusActive)
		s.lastBitrateReport = time.Now()

		go s.worker(s.generation.Load())
	}

	if temporalLayer >= 0 {
		s.bytesForBitrate[temporalLayer] += int64(pktSize)
	}
	s.lock.Unlock()

	if statusChange != StreamStatusChangeNone {
		s.maybeNotifyStatus()
	}
}

// BitrateTemporalCumulative returns the current stream bitrate temporal layer accumulated with lower temporal layers.
func (s *StreamTracker) BitrateTemporalCumulative() []int64 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	// copy and process
	brs := make([]int64, len(s.bitrate))
	copy(brs, s.bitrate[:])

	for i := len(brs) - 1; i >= 1; i-- {
		if brs[i] != 0 {
			for j := i - 1; j >= 0; j-- {
				brs[i] += brs[j]
			}
		}
	}

	// clear higher layers
	for i := 0; i < len(brs); i++ {
		if brs[i] == 0 {
			for j := i + 1; j < len(brs); j++ {
				brs[j] = 0
			}
		}
	}

	return brs
}

func (s *StreamTracker) worker(generation uint32) {
	ticker := time.NewTicker(s.params.StreamTrackerImpl.GetCheckInterval())
	defer ticker.Stop()

	tickerBitrate := time.NewTicker(s.params.BitrateReportInterval)
	defer tickerBitrate.Stop()

	for {
		select {
		case <-ticker.C:
			if generation != s.generation.Load() {
				return
			}
			s.updateStatus()

		case <-tickerBitrate.C:
			if generation != s.generation.Load() {
				return
			}
			s.bitrateReport()
		}
	}
}

func (s *StreamTracker) updateStatus() {
	s.lock.Lock()
	switch s.params.StreamTrackerImpl.CheckStatus() {
	case StreamStatusChangeStopped:
		s.setStatusLocked(StreamStatusStopped)
	case StreamStatusChangeActive:
		s.setStatusLocked(StreamStatusActive)
	}
	s.lock.Unlock()

	s.maybeNotifyStatus()
}

func (s *StreamTracker) bitrateReport() {
	// run this even if paused to drain out bitrate if there are no packets coming in
	s.lock.Lock()
	now := time.Now()
	diff := now.Sub(s.lastBitrateReport)
	s.lastBitrateReport = now

	bitrateAvailabilityChanged := false
	for i := 0; i < len(s.bytesForBitrate); i++ {
		bitrate := int64(float64(s.bytesForBitrate[i]*8) / diff.Seconds())
		if (s.bitrate[i] == 0 && bitrate > 0) || (s.bitrate[i] > 0 && bitrate == 0) {
			bitrateAvailabilityChanged = true
		}
		s.bitrate[i] = bitrate
		s.bytesForBitrate[i] = 0
	}
	s.lock.Unlock()

	if bitrateAvailabilityChanged && s.onBitrateAvailable != nil {
		s.onBitrateAvailable()
	}
}
