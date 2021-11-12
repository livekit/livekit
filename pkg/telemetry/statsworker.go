package telemetry

import (
	"sync"
	"time"

	livekit "github.com/livekit/protocol/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const updateFrequency = time.Second * 10

// StatsWorker handles participant stats
type StatsWorker struct {
	t             *TelemetryService
	roomID        string
	participantID string

	sync.RWMutex
	buffers map[uint32]*buffer.Buffer
	drain   map[uint32]bool

	incoming *DirectionalStats
	outgoing *DirectionalStats

	close chan struct{}
}

type DirectionalStats struct {
	sync.Mutex
	next        *ParticipantStats
	prevPackets uint32
	prevBytes   uint64
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

func NewStatsWorker(t *TelemetryService, roomID, participantID string) *StatsWorker {
	s := &StatsWorker{
		t:             t,
		roomID:        roomID,
		participantID: participantID,

		buffers: make(map[uint32]*buffer.Buffer),
		drain:   make(map[uint32]bool),

		incoming: &DirectionalStats{next: &ParticipantStats{}},
		outgoing: &DirectionalStats{next: &ParticipantStats{}},

		close: make(chan struct{}, 1),
	}
	go s.run()
	return s
}

func (s *StatsWorker) run() {
	for {
		select {
		case <-s.close:
			// drain
			s.Update()
			return
		case <-time.After(updateFrequency):
			s.Update()
		}
	}
}

func (s *StatsWorker) AddBuffer(buffer *buffer.Buffer) {
	s.Lock()
	defer s.Unlock()

	s.buffers[buffer.GetMediaSSRC()] = buffer
}

func (s *StatsWorker) AddRTCP(direction livekit.StreamType, stats *ParticipantStats) {
	ds := s.incoming
	if direction == livekit.StreamType_DOWNSTREAM {
		ds = s.outgoing
	}

	ds.Lock()
	defer ds.Unlock()

	if stats.Delay > ds.next.Delay {
		ds.next.Delay = stats.Delay
	}
	if stats.Jitter > ds.next.Jitter {
		ds.next.Jitter = stats.Jitter
	}
	ds.next.TotalLost += stats.TotalLost
	ds.next.NackCount += stats.NackCount
	ds.next.PliCount += stats.PliCount
	ds.next.FirCount += stats.FirCount
}

func (s *StatsWorker) Update() {
	s.UpdateIncoming()
}

func (s *StatsWorker) UpdateIncoming() {
	s.Lock()
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
	s.Unlock()

	s.incoming.Lock()
	defer s.incoming.Unlock()

	next := s.incoming.next
	s.incoming.next = &ParticipantStats{}

	next.Packets = totalPackets - s.incoming.prevPackets
	s.incoming.prevPackets = totalPackets
	next.Bytes = totalBytes - s.incoming.prevBytes
	s.incoming.prevBytes = totalBytes

	s.t.Report(&livekit.AnalyticsStat{
		Kind:          livekit.StreamType_UPSTREAM,
		TimeStamp:     timestamppb.Now(),
		RoomId:        s.roomID,
		ParticipantId: s.participantID,
		Jitter:        float64(next.Jitter),
		TotalPackets:  uint64(next.Packets),
		PacketLost:    uint64(next.TotalLost),
		Delay:         uint64(next.Delay),
		TotalBytes:    uint64(next.TotalLost),
		NackCount:     next.NackCount,
		PliCount:      next.PliCount,
		FirCount:      next.FirCount,
	})
}

func (s *StatsWorker) RemoveBuffer(ssrc uint32) {
	s.Lock()
	s.drain[ssrc] = true
	s.Unlock()
}

func (s *StatsWorker) Close() {
	close(s.close)
}
