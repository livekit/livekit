package telemetry

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// StatsWorker handles participant stats
type StatsWorker struct {
	ctx                 context.Context
	t                   TelemetryService
	roomID              livekit.RoomID
	roomName            livekit.RoomName
	participantID       livekit.ParticipantID
	participantIdentity livekit.ParticipantIdentity

	lock             sync.RWMutex
	outgoingPerTrack map[livekit.TrackID][]*livekit.AnalyticsStat
	incomingPerTrack map[livekit.TrackID][]*livekit.AnalyticsStat
	closedAt         time.Time
}

func newStatsWorker(
	ctx context.Context,
	t TelemetryService,
	roomID livekit.RoomID,
	roomName livekit.RoomName,
	participantID livekit.ParticipantID,
	identity livekit.ParticipantIdentity,
) *StatsWorker {
	s := &StatsWorker{
		ctx:                 ctx,
		t:                   t,
		roomID:              roomID,
		roomName:            roomName,
		participantID:       participantID,
		participantIdentity: identity,
		outgoingPerTrack:    make(map[livekit.TrackID][]*livekit.AnalyticsStat),
		incomingPerTrack:    make(map[livekit.TrackID][]*livekit.AnalyticsStat),
	}
	return s
}

func (s *StatsWorker) OnTrackStat(trackID livekit.TrackID, direction livekit.StreamType, stat *livekit.AnalyticsStat) {
	s.lock.Lock()
	if direction == livekit.StreamType_DOWNSTREAM {
		s.outgoingPerTrack[trackID] = append(s.outgoingPerTrack[trackID], stat)
	} else {
		s.incomingPerTrack[trackID] = append(s.incomingPerTrack[trackID], stat)
	}
	s.lock.Unlock()
}

func (s *StatsWorker) ParticipantID() livekit.ParticipantID {
	return s.participantID
}

func (s *StatsWorker) Flush() {
	ts := timestamppb.Now()

	s.lock.Lock()
	if !s.closedAt.IsZero() {
		s.lock.Unlock()
		return
	}

	stats := make([]*livekit.AnalyticsStat, 0, len(s.incomingPerTrack)+len(s.outgoingPerTrack))

	incomingPerTrack := s.incomingPerTrack
	s.incomingPerTrack = make(map[livekit.TrackID][]*livekit.AnalyticsStat)

	outgoingPerTrack := s.outgoingPerTrack
	s.outgoingPerTrack = make(map[livekit.TrackID][]*livekit.AnalyticsStat)
	s.lock.Unlock()

	stats = s.collectStats(ts, livekit.StreamType_UPSTREAM, incomingPerTrack, stats)
	stats = s.collectStats(ts, livekit.StreamType_DOWNSTREAM, outgoingPerTrack, stats)
	if len(stats) > 0 {
		s.t.SendStats(s.ctx, stats)
	}
}

func (s *StatsWorker) Close() {
	s.Flush()

	s.lock.Lock()
	s.closedAt = time.Now()
	s.lock.Unlock()
}

func (s *StatsWorker) ClosedAt() time.Time {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.closedAt
}

// -------------------------------------------------------------------------

func (s *StatsWorker) collectStats(
	ts *timestamppb.Timestamp,
	streamType livekit.StreamType,
	perTrack map[livekit.TrackID][]*livekit.AnalyticsStat,
	stats []*livekit.AnalyticsStat,
) []*livekit.AnalyticsStat {
	for trackID, analyticsStats := range perTrack {
		coalesced := coalesce(analyticsStats)
		if coalesced == nil {
			continue
		}

		coalesced.TimeStamp = ts
		coalesced.TrackId = string(trackID)
		coalesced.Kind = streamType
		coalesced.RoomId = string(s.roomID)
		coalesced.ParticipantId = string(s.participantID)
		coalesced.RoomName = string(s.roomName)
		stats = append(stats, coalesced)
	}
	return stats
}

// create a single stream and single video layer post aggregation
func coalesce(stats []*livekit.AnalyticsStat) *livekit.AnalyticsStat {
	if len(stats) == 0 {
		return nil
	}

	// find aggregates across streams
	score := float32(0.0)
	maxRtt := uint32(0)
	maxJitter := uint32(0)
	coalescedVideoLayers := make(map[int32]*livekit.AnalyticsVideoLayer)
	coalescedStream := &livekit.AnalyticsStream{}
	for _, stat := range stats {
		if !isValid(stat) {
			logger.Warnw("telemetry skipping invalid stat", nil, "stat", stat)
			continue
		}

		score += stat.Score
		for _, analyticsStream := range stat.Streams {
			if analyticsStream.Rtt > maxRtt {
				maxRtt = analyticsStream.Rtt
			}

			if analyticsStream.Jitter > maxJitter {
				maxJitter = analyticsStream.Jitter
			}

			coalescedStream.PrimaryPackets += analyticsStream.PrimaryPackets
			coalescedStream.PrimaryBytes += analyticsStream.PrimaryBytes
			coalescedStream.RetransmitPackets += analyticsStream.RetransmitPackets
			coalescedStream.RetransmitBytes += analyticsStream.RetransmitBytes
			coalescedStream.PaddingPackets += analyticsStream.PaddingPackets
			coalescedStream.PaddingBytes += analyticsStream.PaddingBytes
			coalescedStream.PacketsLost += analyticsStream.PacketsLost
			coalescedStream.Frames += analyticsStream.Frames
			coalescedStream.Nacks += analyticsStream.Nacks
			coalescedStream.Plis += analyticsStream.Plis
			coalescedStream.Firs += analyticsStream.Firs

			for _, videoLayer := range analyticsStream.VideoLayers {
				coalescedVideoLayer := coalescedVideoLayers[videoLayer.Layer]
				if coalescedVideoLayer == nil {
					coalescedVideoLayer = proto.Clone(videoLayer).(*livekit.AnalyticsVideoLayer)
					coalescedVideoLayers[videoLayer.Layer] = coalescedVideoLayer
				} else {
					coalescedVideoLayer.Packets += videoLayer.Packets
					coalescedVideoLayer.Bytes += videoLayer.Bytes
					coalescedVideoLayer.Frames += videoLayer.Frames
				}
			}
		}
	}
	coalescedStream.Rtt = maxRtt
	coalescedStream.Jitter = maxJitter

	// whittle it down to one video layer, just the max available layer
	maxVideoLayer := int32(-1)
	for _, coalescedVideoLayer := range coalescedVideoLayers {
		if maxVideoLayer == -1 || maxVideoLayer < coalescedVideoLayer.Layer {
			maxVideoLayer = coalescedVideoLayer.Layer
			coalescedStream.VideoLayers = []*livekit.AnalyticsVideoLayer{coalescedVideoLayer}
		}
	}

	return &livekit.AnalyticsStat{
		Score:   score / float32(len(stats)),
		Streams: []*livekit.AnalyticsStream{coalescedStream},
	}
}

func isValid(stat *livekit.AnalyticsStat) bool {
	for _, analyticsStream := range stat.Streams {
		if int32(analyticsStream.PrimaryPackets) < 0 ||
			int64(analyticsStream.PrimaryBytes) < 0 ||
			int32(analyticsStream.RetransmitPackets) < 0 ||
			int64(analyticsStream.RetransmitBytes) < 0 ||
			int32(analyticsStream.PaddingPackets) < 0 ||
			int64(analyticsStream.PaddingBytes) < 0 ||
			int32(analyticsStream.PacketsLost) < 0 ||
			int32(analyticsStream.Frames) < 0 ||
			int32(analyticsStream.Nacks) < 0 ||
			int32(analyticsStream.Plis) < 0 ||
			int32(analyticsStream.Firs) < 0 {
			return false
		}

		for _, videoLayer := range analyticsStream.VideoLayers {
			if int32(videoLayer.Packets) < 0 ||
				int64(videoLayer.Bytes) < 0 ||
				int32(videoLayer.Frames) < 0 {
				return false
			}
		}
	}

	return true
}
