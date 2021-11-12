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
	next         *ParticipantStats
	totalPackets uint32
	prevPackets  uint32
	totalBytes   uint64
	prevBytes    uint64
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

func (s *StatsWorker) OnDownstreamPacket(bytes int) {
	s.outgoing.Lock()
	defer s.outgoing.Unlock()

	s.outgoing.totalPackets++
	s.outgoing.totalBytes += uint64(bytes)
}

func (s *StatsWorker) OnRTCP(direction livekit.StreamType, stats *ParticipantStats) {
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
	var packetsIn uint32
	var bytesIn uint64

	s.Lock()
	ts := timestamppb.Now()
	for _, buff := range s.buffers {
		stats := buff.GetStats()
		packetsIn += stats.PacketCount
		bytesIn += stats.TotalByte
	}

	if len(s.drain) > 0 {
		for ssrc := range s.drain {
			delete(s.buffers, ssrc)
		}
		s.drain = make(map[uint32]bool)
	}
	s.Unlock()

	s.incoming.Lock()
	s.incoming.totalPackets = packetsIn
	s.incoming.totalBytes = bytesIn
	s.incoming.Unlock()

	stats := make([]*livekit.AnalyticsStat, 0, 2)
	upstream := s.update(livekit.StreamType_UPSTREAM, ts)
	if upstream != nil {
		stats = append(stats, upstream)
	}
	downstream := s.update(livekit.StreamType_DOWNSTREAM, ts)
	if downstream != nil {
		stats = append(stats, downstream)
	}
	s.t.Report(stats)
}

func (s *StatsWorker) update(direction livekit.StreamType, ts *timestamppb.Timestamp) *livekit.AnalyticsStat {
	var ds *DirectionalStats
	if direction == livekit.StreamType_UPSTREAM {
		ds = s.incoming
	} else {
		ds = s.outgoing
	}

	if ds.totalBytes == 0 {
		return nil
	}

	ds.Lock()
	defer ds.Unlock()

	next := ds.next
	ds.next = &ParticipantStats{}

	next.Packets = ds.totalPackets - ds.prevPackets
	ds.prevPackets = ds.totalPackets
	next.Bytes = ds.totalBytes - ds.prevBytes
	ds.prevBytes = ds.totalBytes

	return &livekit.AnalyticsStat{
		Kind:          direction,
		TimeStamp:     ts,
		RoomId:        s.roomID,
		ParticipantId: s.participantID,
		Jitter:        float64(next.Jitter),
		TotalPackets:  uint64(next.Packets),
		PacketLost:    uint64(next.TotalLost),
		Delay:         uint64(next.Delay),
		TotalBytes:    next.Bytes,
		NackCount:     next.NackCount,
		PliCount:      next.PliCount,
		FirCount:      next.FirCount,
	}
}

func (s *StatsWorker) RemoveBuffer(ssrc uint32) {
	s.Lock()
	s.drain[ssrc] = true
	s.Unlock()
}

func (s *StatsWorker) Close() {
	close(s.close)
}
