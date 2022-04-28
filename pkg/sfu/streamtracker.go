package sfu

import (
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/utils"
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

const (
	bitrateReportInterval = 1 * time.Second
)

type StreamTrackerParams struct {
	// number of samples needed per cycle
	SamplesRequired uint32

	// number of cycles needed to be active
	CyclesRequired uint32

	CycleDuration time.Duration

	Logger logger.Logger

	Layer int32
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

	// only access by the same goroutine as Observe
	lastSN uint16

	lastBitrateReport time.Time
	bytesForBitrate   [4]int64
	bitrate           [4]int64

	callbacksQueue *utils.OpsQueue

	isStopped bool
}

func NewStreamTracker(params StreamTrackerParams) *StreamTracker {
	s := &StreamTracker{
		params:         params,
		status:         StreamStatusStopped,
		callbacksQueue: utils.NewOpsQueue(params.Logger),
	}
	s.params.Logger.Debugw("StreamTrackerManager.NewStreamTracker", "layer", s.params.Layer)
	return s
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
		s.callbacksQueue.Enqueue(func() {
			s.onStatusChanged(status)
		})
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
	s.callbacksQueue.Start()
}

func (s *StreamTracker) Stop() {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.isStopped {
		return
	}
	s.isStopped = true

	s.callbacksQueue.Stop()

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
	defer s.lock.Unlock()

	s.paused = paused

	if !paused {
		s.resetLocked()
	}
}

// Observe a packet that's received
func (s *StreamTracker) Observe(sn uint16, temporalLayer int32, pktSize int) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.isStopped || s.paused {
		return
	}

	if !s.initialized {
		// first packet
		s.initialized = true

		s.lastSN = sn
		s.countSinceLast = 1

		s.lastBitrateReport = time.Now()
		if temporalLayer >= 0 {
			s.bytesForBitrate[temporalLayer] += int64(pktSize)
		}

		// declare stream active and start the detection worker
		go s.init()

		return
	}

	// ignore out-of-order SNs
	if (sn - s.lastSN) > uint16(1<<15) {
		return
	}
	s.lastSN = sn
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
	for i := 0; i < len(s.bitrate); i++ {
		brs[i] = s.bitrate[i]
	}

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

	tickerBitrate := time.NewTicker(bitrateReportInterval / 2)
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
	if time.Since(s.lastBitrateReport) < bitrateReportInterval {
		s.lock.Unlock()
		return
	}

	now := time.Now()
	diff := now.Sub(s.lastBitrateReport)
	s.lastBitrateReport = now

	aggLast := int64(0)
	aggNow := int64(0)
	for i := 0; i < len(s.bytesForBitrate); i++ {
		aggLast += s.bitrate[i]
		s.bitrate[i] = int64(float64(s.bytesForBitrate[i]*8) / diff.Seconds())
		aggNow += s.bitrate[i]
		s.bytesForBitrate[i] = 0
	}
	s.lock.Unlock()

	if aggLast == 0 && aggNow > 0 && s.onBitrateAvailable != nil {
		s.onBitrateAvailable()
	}
}
