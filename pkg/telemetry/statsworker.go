package telemetry

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const updateFrequency = time.Second * 10

// StatsWorker handles incoming RTP statistics instead of the stream interceptor
type StatsWorker struct {
	sync.RWMutex
	buffers   map[uint32]*buffer.Buffer
	lastStats *buffer.Stats
	onUpdate  func(diff *buffer.Stats)
	close     chan struct{}
}

func NewStatsWorker(onUpdate func(*buffer.Stats)) *StatsWorker {
	s := &StatsWorker{
		buffers:  make(map[uint32]*buffer.Buffer),
		onUpdate: onUpdate,
		close:    make(chan struct{}, 1),
	}
	go s.run()
	return s
}

func (s *StatsWorker) run() {
	for {
		select {
		case <-s.close:
			return
		case <-time.After(updateFrequency):
			s.onUpdate(s.Calc())
		}
	}
}

func (s *StatsWorker) AddBuffer(buffer *buffer.Buffer) {
	s.Lock()
	s.buffers[buffer.GetMediaSSRC()] = buffer
	s.Unlock()
}

func (s *StatsWorker) RemoveBuffer(ssrc uint32) {
	s.Lock()
	delete(s.buffers, ssrc)
	s.Unlock()
}

func (s *StatsWorker) Calc() *buffer.Stats {
	s.RLock()
	numTracks := len(s.buffers)
	total := &buffer.Stats{}
	for _, buff := range s.buffers {
		stats := buff.GetStats()
		total.PacketCount += stats.PacketCount
		total.TotalByte += stats.TotalByte
		total.LastExpected += stats.LastExpected
		total.LastReceived += stats.LastReceived
		total.LostRate += stats.LostRate
		total.Jitter += stats.Jitter
	}
	s.RUnlock()
	total.LostRate /= float32(numTracks)
	total.Jitter /= float64(numTracks)

	var diff *buffer.Stats
	if s.lastStats != nil {
		diff = &buffer.Stats{
			LastExpected: total.LastExpected - s.lastStats.LastExpected,
			LastReceived: total.LastReceived - s.lastStats.LastReceived,
			PacketCount:  total.PacketCount - s.lastStats.PacketCount,
			TotalByte:    total.TotalByte - s.lastStats.TotalByte,
			LostRate:     total.LostRate,
			Jitter:       total.Jitter,
		}
	} else {
		diff = total
	}

	s.lastStats = diff
	return diff
}

func (s *StatsWorker) Close() {
	close(s.close)
}
