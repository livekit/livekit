package telemetry

import (
	"context"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// StatsWorker handles participant stats
type StatsWorker struct {
	ctx           context.Context
	t             TelemetryReporter
	roomID        livekit.RoomID
	roomName      livekit.RoomName
	participantID livekit.ParticipantID

	outgoingPerTrack map[livekit.TrackID][]*livekit.AnalyticsStat
	incomingPerTrack map[livekit.TrackID][]*livekit.AnalyticsStat
}

func newStatsWorker(
	ctx context.Context,
	t TelemetryReporter,
	roomID livekit.RoomID,
	roomName livekit.RoomName,
	participantID livekit.ParticipantID,
) *StatsWorker {
	s := &StatsWorker{
		ctx:           ctx,
		t:             t,
		roomID:        roomID,
		roomName:      roomName,
		participantID: participantID,

		outgoingPerTrack: make(map[livekit.TrackID][]*livekit.AnalyticsStat),
		incomingPerTrack: make(map[livekit.TrackID][]*livekit.AnalyticsStat),
	}
	return s
}

func (s *StatsWorker) appendOutgoing(trackID livekit.TrackID, stat *livekit.AnalyticsStat) {
	s.outgoingPerTrack[trackID] = append(s.outgoingPerTrack[trackID], stat)
}

func (s *StatsWorker) appendIncoming(trackID livekit.TrackID, stat *livekit.AnalyticsStat) {
	s.incomingPerTrack[trackID] = append(s.incomingPerTrack[trackID], stat)
}

func (s *StatsWorker) OnTrackStat(trackID livekit.TrackID, direction livekit.StreamType, stat *livekit.AnalyticsStat) {
	if direction == livekit.StreamType_DOWNSTREAM {
		s.appendOutgoing(trackID, stat)
	} else {
		s.appendIncoming(trackID, stat)
	}
}

func (s *StatsWorker) Update() {
	ts := timestamppb.Now()

	stats := make([]*livekit.AnalyticsStat, 0, len(s.incomingPerTrack)+len(s.outgoingPerTrack))
	stats = s.collectUpstreamStats(ts, stats)
	stats = s.collectDownstreamStats(ts, stats)
	if len(stats) > 0 {
		s.t.Report(s.ctx, stats)
	}
}

func (s *StatsWorker) collectDownstreamStats(ts *timestamppb.Timestamp, stats []*livekit.AnalyticsStat) []*livekit.AnalyticsStat {
	for trackID, analyticsStats := range s.outgoingPerTrack {
		analyticsStat := coalesce(analyticsStats)
		if analyticsStat == nil {
			continue
		}

		s.patch(analyticsStat, ts, trackID, livekit.StreamType_DOWNSTREAM)
		stats = append(stats, analyticsStat)
	}
	s.outgoingPerTrack = make(map[livekit.TrackID][]*livekit.AnalyticsStat, 0)

	return stats
}

func (s *StatsWorker) collectUpstreamStats(ts *timestamppb.Timestamp, stats []*livekit.AnalyticsStat) []*livekit.AnalyticsStat {
	for trackID, analyticsStats := range s.incomingPerTrack {
		analyticsStat := coalesce(analyticsStats)
		if analyticsStat == nil {
			continue
		}

		s.patch(analyticsStat, ts, trackID, livekit.StreamType_UPSTREAM)
		stats = append(stats, analyticsStat)
	}
	s.incomingPerTrack = make(map[livekit.TrackID][]*livekit.AnalyticsStat, 0)

	return stats
}

func (s *StatsWorker) patch(
	analyticsStat *livekit.AnalyticsStat,
	ts *timestamppb.Timestamp,
	trackID livekit.TrackID,
	kind livekit.StreamType,
) {
	analyticsStat.TimeStamp = ts
	analyticsStat.TrackId = string(trackID)
	analyticsStat.Kind = kind
	analyticsStat.RoomId = string(s.roomID)
	analyticsStat.ParticipantId = string(s.participantID)
	analyticsStat.RoomName = string(s.roomName)
}

func (s *StatsWorker) Close() {
	s.Update()
}

func coalesce(stats []*livekit.AnalyticsStat) *livekit.AnalyticsStat {
	if len(stats) == 0 {
		return nil
	}

	// average score of all available stats
	score := float32(0.0)
	for _, stat := range stats {
		score += stat.Score
	}
	score = score / float32(len(stats))

	// aggregate streams across all stats
	maxRTT := make(map[uint32]uint32)
	maxJitter := make(map[uint32]uint32)
	analyticsStreams := make(map[uint32]*livekit.AnalyticsStream)
	for _, stat := range stats {
		//
		// For each stream (identified by SSRC) consolidate reports.
		// For cumulative stats, take the latest report.
		// For instantaneous stats, take maximum (or some other appropriate representation)
		//
		for _, stream := range stat.Streams {
			ssrc := stream.Ssrc
			analyticsStream := analyticsStreams[ssrc]
			if analyticsStream == nil {
				analyticsStreams[ssrc] = stream
				maxRTT[ssrc] = stream.Rtt
				maxJitter[ssrc] = stream.Jitter
				continue
			}

			if stream.TotalPrimaryPackets <= analyticsStream.TotalPrimaryPackets {
				// total count should be monotonically increasing
				continue
			}

			analyticsStreams[ssrc] = stream
			if stream.Rtt > maxRTT[ssrc] {
				maxRTT[ssrc] = stream.Rtt
			}

			if stream.Jitter > maxJitter[ssrc] {
				maxJitter[ssrc] = stream.Jitter
			}
		}
	}

	streams := make([]*livekit.AnalyticsStream, 0, len(analyticsStreams))
	for ssrc, analyticsStream := range analyticsStreams {
		stream := proto.Clone(analyticsStream).(*livekit.AnalyticsStream)
		stream.Rtt = maxRTT[ssrc]
		stream.Jitter = maxJitter[ssrc]

		streams = append(streams, stream)
	}

	return &livekit.AnalyticsStat{
		Score:   score,
		Streams: streams,
	}
}
