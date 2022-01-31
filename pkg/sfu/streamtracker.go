package sfu

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/utils"
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

// StreamTracker keeps track of packet flow and ensures a particular up track is consistently producing
// It runs its own goroutine for detection, and fires OnStatusChanged callback
type StreamTracker struct {
	// number of samples needed per cycle
	samplesRequired uint32
	// number of cycles needed to be active
	cyclesRequired uint64
	cycleDuration  time.Duration

	onStatusChanged func(status StreamStatus)

	paused         atomicBool
	countSinceLast uint32 // number of packets received since last check
	generation     atomicUint32

	initMu      sync.Mutex
	initialized bool

	statusMu sync.RWMutex
	status   StreamStatus

	// only access within detectWorker
	cycleCount uint64

	// only access by the same goroutine as Observe
	lastSN uint16

	isStopped utils.AtomicFlag
}

func NewStreamTracker(samplesRequired uint32, cyclesRequired uint64, cycleDuration time.Duration) *StreamTracker {
	s := &StreamTracker{
		samplesRequired: samplesRequired,
		cyclesRequired:  cyclesRequired,
		cycleDuration:   cycleDuration,
		status:          StreamStatusStopped,
	}
	return s
}

func (s *StreamTracker) OnStatusChanged(f func(status StreamStatus)) {
	s.onStatusChanged = f
}

func (s *StreamTracker) Status() StreamStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()

	return s.status
}

func (s *StreamTracker) maybeSetStatus(status StreamStatus) {
	changed := false
	s.statusMu.Lock()
	if s.status != status {
		s.status = status
		changed = true
	}
	s.statusMu.Unlock()

	if changed && s.onStatusChanged != nil {
		s.onStatusChanged(status)
	}
}

func (s *StreamTracker) maybeSetActive() {
	s.maybeSetStatus(StreamStatusActive)
}

func (s *StreamTracker) maybeSetStopped() {
	s.maybeSetStatus(StreamStatusStopped)
}

func (s *StreamTracker) init() {
	s.maybeSetActive()

	go s.detectWorker(s.generation.get())
}

func (s *StreamTracker) Start() {
}

func (s *StreamTracker) Stop() {
	if !s.isStopped.TrySet(true) {
		return
	}

	// bump generation to trigger exit of worker
	s.generation.add(1)
}

func (s *StreamTracker) Reset() {
	if s.isStopped.Get() {
		return
	}

	// bump generation to trigger exit of current worker
	s.generation.add(1)

	atomic.StoreUint32(&s.countSinceLast, 0)
	s.cycleCount = 0

	s.initMu.Lock()
	s.initialized = false
	s.initMu.Unlock()

	s.statusMu.Lock()
	s.status = StreamStatusStopped
	s.statusMu.Unlock()
}

func (s *StreamTracker) SetPaused(paused bool) {
	s.paused.set(paused)
}

// Observe a packet that's received
func (s *StreamTracker) Observe(sn uint16) {
	if s.paused.get() {
		return
	}

	s.initMu.Lock()
	if !s.initialized {
		// first packet
		s.initialized = true
		s.initMu.Unlock()

		s.lastSN = sn
		atomic.AddUint32(&s.countSinceLast, 1)

		// declare stream active and start the detection worker
		go s.init()

		return
	}
	s.initMu.Unlock()

	// ignore out-of-order SNs
	if (sn - s.lastSN) > uint16(1<<15) {
		return
	}
	s.lastSN = sn
	atomic.AddUint32(&s.countSinceLast, 1)
}

func (s *StreamTracker) detectWorker(generation uint32) {
	ticker := time.NewTicker(s.cycleDuration)

	for {
		<-ticker.C
		if generation != s.generation.get() {
			return
		}

		s.detectChanges()
	}
}

func (s *StreamTracker) detectChanges() {
	if s.paused.get() {
		return
	}

	if atomic.LoadUint32(&s.countSinceLast) >= s.samplesRequired {
		s.cycleCount += 1
	} else {
		s.cycleCount = 0
	}

	if s.cycleCount == 0 {
		// flip to stopped
		s.maybeSetStopped()
	} else if s.cycleCount >= s.cyclesRequired {
		// flip to active
		s.maybeSetActive()
	}

	atomic.StoreUint32(&s.countSinceLast, 0)
}
