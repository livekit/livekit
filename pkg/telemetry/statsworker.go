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
	drain     map[uint32]bool
	lastStats *ParticipantStats
	nackTotal int32
	pliTotal  int32
	firTotal  int32
	onUpdate  func(diff *ParticipantStats)

	running bool
	close   chan struct{}
}

type ParticipantStats struct {
	Packets   uint32
	Bytes     uint64
	Jitter    float64
	NackCount int32
	PliCount  int32
	FirCount  int32
}

func NewStatsWorker(onUpdate func(*ParticipantStats)) *StatsWorker {
	return &StatsWorker{
		buffers:  make(map[uint32]*buffer.Buffer),
		drain:    make(map[uint32]bool),
		onUpdate: onUpdate,
		close:    make(chan struct{}, 1),
	}
}

func (s *StatsWorker) run() {
	for {
		select {
		case <-s.close:
			// drain
			s.onUpdate(s.Calc())
			return
		case <-time.After(updateFrequency):
			s.onUpdate(s.Calc())
		}
	}
}

func (s *StatsWorker) AddBuffer(buffer *buffer.Buffer) {
	s.Lock()
	s.buffers[buffer.GetMediaSSRC()] = buffer
	if !s.running {
		s.running = true
		go s.run()
	}
	s.Unlock()
}

func (s *StatsWorker) AddRTCP(nack, pli, fir int32) {
	s.Lock()
	s.nackTotal += nack
	s.pliTotal += pli
	s.firTotal += fir
	s.Unlock()
}

func (s *StatsWorker) Calc() *ParticipantStats {
	s.RLock()
	total := &ParticipantStats{
		NackCount: s.nackTotal,
		PliCount:  s.pliTotal,
		FirCount:  s.firTotal,
	}
	for _, buff := range s.buffers {
		stats := buff.GetStats()
		total.Packets += stats.PacketCount
		total.Bytes += stats.TotalByte
		if stats.Jitter > total.Jitter {
			total.Jitter = stats.Jitter
		}
	}
	drain := len(s.drain) > 0
	s.RUnlock()

	if drain {
		s.Lock()
		for ssrc := range s.drain {
			delete(s.buffers, ssrc)
		}
		s.drain = make(map[uint32]bool)
		s.Unlock()
	}

	var diff *ParticipantStats
	if s.lastStats != nil {
		diff = &ParticipantStats{
			Packets:   total.Packets - s.lastStats.Packets,
			Bytes:     total.Bytes - s.lastStats.Bytes,
			Jitter:    total.Jitter,
			NackCount: total.NackCount - s.lastStats.NackCount,
			PliCount:  total.PliCount - s.lastStats.PliCount,
			FirCount:  total.FirCount - s.lastStats.FirCount,
		}
	} else {
		diff = total
	}
	s.lastStats = total

	return diff
}

func (s *StatsWorker) RemoveBuffer(ssrc uint32) {
	s.Lock()
	s.drain[ssrc] = true
	s.Unlock()
}

func (s *StatsWorker) Close() {
	close(s.close)
}
