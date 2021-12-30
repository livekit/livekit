package telemetry

import (
	"context"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

// StatsWorker handles participant stats
type StatsWorker struct {
	ctx           context.Context
	t             TelemetryReporter
	roomID        livekit.RoomID
	roomName      livekit.RoomName
	participantID livekit.ParticipantID

	upstreamBuffers      map[livekit.TrackID][]*buffer.Buffer
	drainUpstreamBuffers map[livekit.TrackID]bool

	outgoingPerTrack map[livekit.TrackID]*Stats
	incomingPerTrack map[livekit.TrackID]*Stats
}

type Stats struct {
	next             *livekit.AnalyticsStat
	totalPackets     uint32
	prevPackets      uint32
	totalBytes       uint64
	prevBytes        uint64
	totalPacketsLost uint64
	prevPacketsLost  uint64
}

func newStatsWorker(ctx context.Context, t TelemetryReporter, roomID livekit.RoomID, roomName livekit.RoomName, participantID livekit.ParticipantID) *StatsWorker {
	s := &StatsWorker{
		ctx:           ctx,
		t:             t,
		roomID:        roomID,
		roomName:      roomName,
		participantID: participantID,

		upstreamBuffers:      make(map[livekit.TrackID][]*buffer.Buffer),
		drainUpstreamBuffers: make(map[livekit.TrackID]bool),

		outgoingPerTrack: make(map[livekit.TrackID]*Stats),
		incomingPerTrack: make(map[livekit.TrackID]*Stats),
	}
	return s
}

func (s *StatsWorker) AddBuffer(trackID livekit.TrackID, buffer *buffer.Buffer) {
	s.upstreamBuffers[trackID] = append(s.upstreamBuffers[trackID], buffer)
}

func (s *StatsWorker) OnDownstreamPacket(trackID livekit.TrackID, bytes int) {
	s.getOrCreateOutgoingStatsIfEmpty(trackID).totalBytes += uint64(bytes)
	s.getOrCreateOutgoingStatsIfEmpty(trackID).totalPackets++
}

func (s *StatsWorker) getOrCreateOutgoingStatsIfEmpty(trackID livekit.TrackID) *Stats {
	if s.outgoingPerTrack[trackID] == nil {
		s.outgoingPerTrack[trackID] = &Stats{next: &livekit.AnalyticsStat{
			Kind:          livekit.StreamType_DOWNSTREAM,
			RoomId:        s.roomID,
			ParticipantId: s.participantID,
			RoomName:      s.roomName,
		}}
	}
	return s.outgoingPerTrack[trackID]
}

func (s *StatsWorker) getOrCreateIncomingStatsIfEmpty(trackID livekit.TrackID) *Stats {
	if s.incomingPerTrack[trackID] == nil {
		s.incomingPerTrack[trackID] = &Stats{next: &livekit.AnalyticsStat{
			Kind:          livekit.StreamType_UPSTREAM,
			RoomId:        s.roomID,
			ParticipantId: s.participantID,
			RoomName:      s.roomName,
		}}
	}
	return s.incomingPerTrack[trackID]
}

func (s *StatsWorker) OnRTCP(trackID livekit.TrackID, direction livekit.StreamType, stats *livekit.AnalyticsStat) {
	var ds *Stats
	if direction == livekit.StreamType_DOWNSTREAM {
		ds = s.getOrCreateOutgoingStatsIfEmpty(trackID)
	} else {
		ds = s.getOrCreateIncomingStatsIfEmpty(trackID)
	}
	ds.totalPacketsLost = stats.PacketLost

	if stats.Delay > ds.next.Delay {
		ds.next.Delay = stats.Delay
	}
	if stats.Jitter > ds.next.Jitter {
		ds.next.Jitter = stats.Jitter
	}
	ds.next.NackCount += stats.NackCount
	ds.next.PliCount += stats.PliCount
	ds.next.FirCount += stats.FirCount
}

func (s *StatsWorker) calculateTotalBytesPackets(allBuffers []*buffer.Buffer) (totalBytes uint64, totalPackets uint32) {
	totalBytes = 0
	totalPackets = 0

	for _, buffer := range allBuffers {
		totalBytes += buffer.GetStats().TotalByte
		totalPackets += buffer.GetStats().PacketCount
	}
	return totalBytes, totalPackets
}

func (s *StatsWorker) Update() {
	ts := timestamppb.Now()
	stats := make([]*livekit.AnalyticsStat, 0)

	stats = s.collectUpstreamStats(ts, stats)
	stats = s.collectDownstreamStats(ts, stats)

	s.t.Report(s.ctx, stats)
}

func (s *StatsWorker) collectDownstreamStats(ts *timestamppb.Timestamp, stats []*livekit.AnalyticsStat) []*livekit.AnalyticsStat {
	for trackID, trackDownStreamStats := range s.outgoingPerTrack {
		analyticsStat := s.update(trackDownStreamStats, ts)
		if analyticsStat != nil {
			analyticsStat.TrackId = trackID
			stats = append(stats, analyticsStat)
		}
	}
	return stats
}

func (s *StatsWorker) collectUpstreamStats(ts *timestamppb.Timestamp, stats []*livekit.AnalyticsStat) []*livekit.AnalyticsStat {
	for trackID, buffers := range s.upstreamBuffers {
		totalBytes, totalPackets := s.calculateTotalBytesPackets(buffers)

		s.getOrCreateIncomingStatsIfEmpty(trackID).totalBytes = totalBytes
		s.getOrCreateIncomingStatsIfEmpty(trackID).totalPackets = totalPackets

		analyticsStats := s.update(s.incomingPerTrack[trackID], ts)
		if analyticsStats != nil {
			analyticsStats.TrackId = trackID
			stats = append(stats, analyticsStats)
		}
	}

	if len(s.drainUpstreamBuffers) > 0 {
		for trackID := range s.drainUpstreamBuffers {
			delete(s.upstreamBuffers, trackID)
			delete(s.incomingPerTrack, trackID)
		}
		s.drainUpstreamBuffers = make(map[livekit.TrackID]bool)
	}
	return stats
}

func (s *StatsWorker) update(stats *Stats, ts *timestamppb.Timestamp) *livekit.AnalyticsStat {
	if stats.totalBytes == 0 {
		return nil
	}

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
	next.PacketLost = stats.totalPacketsLost - stats.prevPacketsLost

	stats.prevPackets = stats.totalPackets
	stats.prevBytes = stats.totalBytes
	stats.prevPacketsLost = stats.totalPacketsLost

	return next
}

func (s *StatsWorker) RemoveBuffer(trackID livekit.TrackID) {
	s.drainUpstreamBuffers[trackID] = true
}

func (s *StatsWorker) Close() {
	s.Update()
}
