package sfu

import (
	"sync/atomic"
	"time"
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

// StreamTracker keeps track of packet flow and ensures a particular uptrack is consistently producing
// It runs its own goroutine for detection, and fires OnStatusChanged callback
type StreamTracker struct {
	// number of samples needed per cycle
	SamplesRequired uint32
	// number of cycles needed to be active
	CyclesRequired  uint64
	CycleDuration   time.Duration
	OnStatusChanged func(StreamStatus)
	paused          atomicBool
	status          atomicInt32 // stores StreamStatus
	countSinceLast  uint32      // number of packets received since last check
	running         chan struct{}

	// only access within detectWorker
	cycleCount uint64

	// only access by the same goroutine as Observe
	lastSN uint16
}

func NewStreamTracker() *StreamTracker {
	s := &StreamTracker{
		SamplesRequired: 5,
		CyclesRequired:  60, // 30s of continuous stream
		CycleDuration:   500 * time.Millisecond,
	}
	s.status.set(int32(StreamStatusActive))
	return s
}

func (s *StreamTracker) Status() StreamStatus {
	return StreamStatus(s.status.get())
}

func (s *StreamTracker) setStatus(status StreamStatus) {
	if status != s.Status() {
		s.status.set(int32(status))
		if s.OnStatusChanged != nil {
			s.OnStatusChanged(status)
		}
	}
}

func (s *StreamTracker) Start() {
	if s.isRunning() {
		return
	}
	s.running = make(chan struct{})
	go s.detectWorker()
}

func (s *StreamTracker) Stop() {
	if s.running != nil {
		close(s.running)
		s.running = nil
	}
}

func (s *StreamTracker) SetPaused(paused bool) {
	s.paused.set(paused)
}

func (s *StreamTracker) isRunning() bool {
	if s.running == nil {
		return false
	}
	select {
	case <-s.running:
		return false
	default:
		return true
	}
}

// Observe a packet that's received
func (s *StreamTracker) Observe(sn uint16) {
	if s.paused.get() {
		return
	}
	// ignore out-of-order SNs
	if (sn - s.lastSN) > uint16(1<<15) {
		return
	}
	s.lastSN = sn
	atomic.AddUint32(&s.countSinceLast, 1)
}

func (s *StreamTracker) detectWorker() {
	ticker := time.NewTicker(s.CycleDuration)

	for s.isRunning() {
		<-ticker.C
		if !s.isRunning() {
			return
		}

		s.detectChanges()
	}
}

func (s *StreamTracker) detectChanges() {
	if s.paused.get() {
		return
	}

	if atomic.LoadUint32(&s.countSinceLast) >= s.SamplesRequired {
		s.cycleCount += 1
	} else {
		s.cycleCount = 0
	}

	if s.cycleCount == 0 && s.Status() == StreamStatusActive {
		// flip to stopped
		s.setStatus(StreamStatusStopped)
	} else if s.cycleCount >= s.CyclesRequired && s.Status() == StreamStatusStopped {
		// flip to active
		s.setStatus(StreamStatusActive)
	}

	atomic.StoreUint32(&s.countSinceLast, 0)
}
