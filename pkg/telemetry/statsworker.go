package telemetry

import (
	"context"
	"sync"
	"time"

	livekit "github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const updateFrequency = time.Second * 10

// StatsWorker handles participant stats
type StatsWorker struct {
	ctx           context.Context
	t             TelemetryService
	roomID        string
	roomName      string
	participantID string

	sync.RWMutex
	buffers map[uint32]*buffer.Buffer
	drain   map[uint32]bool

	incoming *Stats
	outgoing *Stats

	close chan struct{}
}

type Stats struct {
	sync.Mutex
	next         *livekit.AnalyticsStat
	totalPackets uint32
	prevPackets  uint32
	totalBytes   uint64
	prevBytes    uint64
}

func newStatsWorker(ctx context.Context, t TelemetryService, roomID, roomName, participantID string) *StatsWorker {
	s := &StatsWorker{
		ctx:           ctx,
		t:             t,
		roomID:        roomID,
		roomName:      roomName,
		participantID: participantID,

		buffers: make(map[uint32]*buffer.Buffer),
		drain:   make(map[uint32]bool),

		incoming: &Stats{next: &livekit.AnalyticsStat{
			Kind:          livekit.StreamType_UPSTREAM,
			RoomId:        roomID,
			ParticipantId: participantID,
			RoomName:      roomName,
		}},
		outgoing: &Stats{next: &livekit.AnalyticsStat{
			Kind:          livekit.StreamType_DOWNSTREAM,
			RoomId:        roomID,
			ParticipantId: participantID,
			RoomName:      roomName,
		}},

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

func (s *StatsWorker) OnRTCP(direction livekit.StreamType, stats *livekit.AnalyticsStat) {
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
	ds.next.PacketLost += stats.PacketLost
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
	upstream := s.update(s.incoming, ts)
	if upstream != nil {
		stats = append(stats, upstream)
	}
	downstream := s.update(s.outgoing, ts)
	if downstream != nil {
		stats = append(stats, downstream)
	}

	s.t.Report(s.ctx, stats)
}

func (s *StatsWorker) update(stats *Stats, ts *timestamppb.Timestamp) *livekit.AnalyticsStat {
	if stats.totalBytes == 0 {
		return nil
	}

	stats.Lock()
	defer stats.Unlock()

	next := stats.next
	stats.next = &livekit.AnalyticsStat{
		Kind:          next.Kind,
		RoomId:        s.roomID,
		ParticipantId: s.participantID,
		RoomName:      s.roomName,
	}

	next.TimeStamp = ts
	next.TotalPackets = uint64(stats.totalPackets - stats.prevPackets)
	next.TotalBytes = stats.totalBytes - stats.prevBytes

	stats.prevPackets = stats.totalPackets
	stats.prevBytes = stats.totalBytes

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
