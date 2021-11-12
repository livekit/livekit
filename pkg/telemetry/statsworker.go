package telemetry

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const updateFrequency = time.Second * 10

// StatsWorker handles publisher stats
type StatsWorker struct {
	sync.RWMutex
	buffers  map[uint32]*buffer.Buffer
	drain    map[uint32]bool
	onUpdate func(diff *ParticipantStats)

	next        *ParticipantStats
	prevPackets uint32
	prevBytes   uint64

	running bool
	close   chan struct{}
}

type ParticipantStats struct {
	Packets   uint32
	Bytes     uint64
	Delay     uint32
	Jitter    uint32
	TotalLost uint32
	NackCount int32
	PliCount  int32
	FirCount  int32
}

func NewStatsWorker(onUpdate func(*ParticipantStats)) *StatsWorker {
	return &StatsWorker{
		buffers:  make(map[uint32]*buffer.Buffer),
		drain:    make(map[uint32]bool),
		onUpdate: onUpdate,
		next:     &ParticipantStats{},
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
	defer s.Unlock()

	s.buffers[buffer.GetMediaSSRC()] = buffer
	if !s.running {
		s.running = true
		go s.run()
	}
}

func (s *StatsWorker) AddRTCP(delay, jitter, totalLost uint32, nack, pli, fir int32) {
	s.Lock()
	defer s.Unlock()

	if delay > s.next.Delay {
		s.next.Delay = delay
	}
	if jitter > s.next.Jitter {
		s.next.Jitter = jitter
	}
	s.next.TotalLost += totalLost
	s.next.NackCount += nack
	s.next.PliCount += pli
	s.next.FirCount += fir
}

func (s *StatsWorker) Calc() *ParticipantStats {
	s.Lock()
	defer s.Unlock()

	var totalPackets uint32
	var totalBytes uint64
	for _, buff := range s.buffers {
		stats := buff.GetStats()
		totalPackets += stats.PacketCount
		totalBytes += stats.TotalByte
	}

	if len(s.drain) > 0 {
		for ssrc := range s.drain {
			delete(s.buffers, ssrc)
		}
		s.drain = make(map[uint32]bool)
	}

	next := s.next
	s.next = &ParticipantStats{}

	next.Packets = totalPackets - s.prevPackets
	s.prevPackets = totalPackets
	next.Bytes = totalBytes - s.prevBytes
	s.prevBytes = totalBytes

	return next
}

func (s *StatsWorker) RemoveBuffer(ssrc uint32) {
	s.Lock()
	s.drain[ssrc] = true
	s.Unlock()
}

func (s *StatsWorker) Close() {
	close(s.close)
}
