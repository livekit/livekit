package sfu

import (
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/protocol/logger"
)

type StreamStatus int32

func (s StreamStatus) String() string {
	switch s {
	case StreamStatusStopped:
		return "stopped"
	case StreamStatusActive:
		return "active"
	default:
		return "unknown"
	}
}

const (
	StreamStatusStopped StreamStatus = 0
	StreamStatusActive  StreamStatus = 1
)

type StreamTrackerParams struct {
	// number of samples needed per cycle
	SamplesRequired uint32

	// number of cycles needed to be active
	CyclesRequired uint32

	CycleDuration time.Duration

	BitrateReportInterval time.Duration

	Logger logger.Logger
}

// StreamTracker keeps track of packet flow and ensures a particular up track is consistently producing
// It runs its own goroutine for detection, and fires OnStatusChanged callback
type StreamTracker struct {
	params StreamTrackerParams

	onStatusChanged    func(status StreamStatus)
	onBitrateAvailable func()

	lock sync.RWMutex

	paused         bool
	countSinceLast uint32 // number of packets received since last check
	generation     atomic.Uint32

	initialized bool

	status StreamStatus

	// only access within detectWorker
	cycleCount uint32

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

func (s *StreamTracker) maybeSetStatus(status StreamStatus) (StreamStatus, bool) {
	changed := false
	if s.status != status {
		s.status = status
		changed = true
	}

	return status, changed
}

func (s *StreamTracker) maybeNotifyStatus(status StreamStatus, changed bool) {
	if changed && s.onStatusChanged != nil {
		s.onStatusChanged(status)
	}
}

func (s *StreamTracker) init() {
	s.lock.Lock()
	status, changed := s.maybeSetStatus(StreamStatusActive)
	s.lock.Unlock()

	s.maybeNotifyStatus(status, changed)

	go s.detectWorker(s.generation.Load())
}

func (s *StreamTracker) Start() {
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
}

func (s *StreamTracker) Reset() {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.isStopped {
		return
	}

	s.resetLocked()
}

func (s *StreamTracker) resetLocked() {
	// bump generation to trigger exit of current worker
	s.generation.Inc()

	s.countSinceLast = 0
	s.cycleCount = 0

	s.initialized = false

	s.status = StreamStatusStopped

	for i := 0; i < len(s.bytesForBitrate); i++ {
		s.bytesForBitrate[i] = 0
	}
	for i := 0; i < len(s.bitrate); i++ {
		s.bitrate[i] = 0
	}
}

func (s *StreamTracker) SetPaused(paused bool) {
	s.lock.Lock()

	s.paused = paused

	status := s.status
	changed := false
	if !paused {
		s.resetLocked()
	} else {
		// bump generation to trigger exit of current worker
		s.generation.Inc()

		status, changed = s.maybeSetStatus(StreamStatusStopped)
	}
	s.lock.Unlock()

	s.maybeNotifyStatus(status, changed)
}

// Observe a packet that's received
func (s *StreamTracker) Observe(sn uint16, temporalLayer int32, pktSize int, payloadSize int) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.isStopped || s.paused || payloadSize == 0 {
		return
	}

	if !s.initialized {
		// first packet
		s.initialized = true

		s.countSinceLast = 1

		s.lastBitrateReport = time.Now()
		if temporalLayer >= 0 {
			s.bytesForBitrate[temporalLayer] += int64(pktSize)
		}

		// declare stream active and start the detection worker
		go s.init()

		return
	}

	s.countSinceLast++

	if temporalLayer >= 0 {
		s.bytesForBitrate[temporalLayer] += int64(pktSize)
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

	return brs
}

func (s *StreamTracker) detectWorker(generation uint32) {
	ticker := time.NewTicker(s.params.CycleDuration)
	defer ticker.Stop()

	tickerBitrate := time.NewTicker(s.params.BitrateReportInterval)
	defer tickerBitrate.Stop()

	for {
		select {
		case <-ticker.C:
			if generation != s.generation.Load() {
				return
			}
			s.detectChanges()

		case <-tickerBitrate.C:
			if generation != s.generation.Load() {
				return
			}
			s.bitrateReport()
		}
	}
}

func (s *StreamTracker) detectChanges() {
	s.lock.Lock()
	if s.countSinceLast >= s.params.SamplesRequired {
		s.cycleCount++
	} else {
		s.cycleCount = 0
	}

	status := s.status
	changed := false
	if s.cycleCount == 0 {
		// flip to stopped
		status, changed = s.maybeSetStatus(StreamStatusStopped)
	} else if s.cycleCount >= s.params.CyclesRequired {
		// flip to active
		status, changed = s.maybeSetStatus(StreamStatusActive)
	}

	s.countSinceLast = 0
	s.lock.Unlock()

	s.maybeNotifyStatus(status, changed)
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
