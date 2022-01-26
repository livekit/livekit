package telemetry

import (
	"context"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// StatsWorker handles participant stats
type StatsWorker struct {
	ctx           context.Context
	t             TelemetryReporter
	roomID        livekit.RoomID
	roomName      livekit.RoomName
	participantID livekit.ParticipantID

	drainStats       map[livekit.TrackID]bool
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
	connectionScore  float32
}

func newStatsWorker(ctx context.Context, t TelemetryReporter, roomID livekit.RoomID, roomName livekit.RoomName, participantID livekit.ParticipantID) *StatsWorker {
	s := &StatsWorker{
		ctx:           ctx,
		t:             t,
		roomID:        roomID,
		roomName:      roomName,
		participantID: participantID,

		outgoingPerTrack: make(map[livekit.TrackID]*Stats),
		incomingPerTrack: make(map[livekit.TrackID]*Stats),
		drainStats:       make(map[livekit.TrackID]bool),
	}
	return s
}

func (s *StatsWorker) getOrCreateOutgoingStatsIfEmpty(trackID livekit.TrackID) *Stats {
	if s.outgoingPerTrack[trackID] == nil {
		s.outgoingPerTrack[trackID] = &Stats{next: &livekit.AnalyticsStat{
			Kind:          livekit.StreamType_DOWNSTREAM,
			RoomId:        string(s.roomID),
			ParticipantId: string(s.participantID),
			RoomName:      string(s.roomName),
		}}
	}
	return s.outgoingPerTrack[trackID]
}

func (s *StatsWorker) getOrCreateIncomingStatsIfEmpty(trackID livekit.TrackID) *Stats {
	if s.incomingPerTrack[trackID] == nil {
		s.incomingPerTrack[trackID] = &Stats{next: &livekit.AnalyticsStat{
			Kind:          livekit.StreamType_UPSTREAM,
			RoomId:        string(s.roomID),
			ParticipantId: string(s.participantID),
			RoomName:      string(s.roomName),
		}}
	}
	return s.incomingPerTrack[trackID]
}

func (s *StatsWorker) OnTrackStat(trackID livekit.TrackID, direction livekit.StreamType, stats *livekit.AnalyticsStat) {
	var ds *Stats
	if direction == livekit.StreamType_DOWNSTREAM {
		ds = s.getOrCreateOutgoingStatsIfEmpty(trackID)
	} else {
		ds = s.getOrCreateIncomingStatsIfEmpty(trackID)
	}
	ds.totalPacketsLost = stats.PacketLost
	ds.totalPackets = uint32(stats.TotalPackets)
	ds.totalBytes = stats.TotalBytes

	if stats.Rtt > ds.next.Rtt {
		ds.next.Rtt = stats.Rtt
	}
	if stats.Jitter > ds.next.Jitter {
		ds.next.Jitter = stats.Jitter
	}
	ds.next.NackCount += stats.NackCount
	ds.next.PliCount += stats.PliCount
	ds.next.FirCount += stats.FirCount
	// average out scores received in this interval
	ds.next.ConnectionScore = ds.next.ConnectionScore + stats.ConnectionScore/2
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
			analyticsStat.TrackId = string(trackID)
			stats = append(stats, analyticsStat)
		}
	}
	if len(s.drainStats) > 0 {
		for trackID := range s.drainStats {
			delete(s.outgoingPerTrack, trackID)
			delete(s.incomingPerTrack, trackID)
		}
		s.drainStats = make(map[livekit.TrackID]bool)
	}

	return stats
}

func (s *StatsWorker) collectUpstreamStats(ts *timestamppb.Timestamp, stats []*livekit.AnalyticsStat) []*livekit.AnalyticsStat {
	for trackID, trackUpStreamStats := range s.incomingPerTrack {
		analyticsStat := s.update(trackUpStreamStats, ts)
		if analyticsStat != nil {
			analyticsStat.TrackId = string(trackID)
			stats = append(stats, analyticsStat)
		}
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
		RoomId:        string(s.roomID),
		ParticipantId: string(s.participantID),
		RoomName:      string(s.roomName),
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

func (s *StatsWorker) Close() {
	s.Update()
}

func (s *StatsWorker) RemoveStats(trackID livekit.TrackID) {
	s.drainStats[trackID] = true
}
