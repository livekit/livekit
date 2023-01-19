package telemetry

import (
	"fmt"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
)

const statsReportInterval = 10 * time.Second

type BytesTrackType string

const (
	BytesTrackTypeData   BytesTrackType = "DT"
	BytesTrackTypeSignal BytesTrackType = "SG"
)

// stats for signal and data channel
type BytesTrackStats struct {
	trackID         livekit.TrackID
	pID             livekit.ParticipantID
	send, recv      atomic.Uint64
	lastStatsReport atomic.Value // *time.Time
	telemetry       TelemetryService
}

func NewBytesTrackStats(trackID livekit.TrackID, pID livekit.ParticipantID, telemetry TelemetryService) *BytesTrackStats {
	s := &BytesTrackStats{
		trackID:   trackID,
		pID:       pID,
		telemetry: telemetry,
	}
	now := time.Now()
	s.lastStatsReport.Store(&now)
	return s
}

func (s *BytesTrackStats) AddBytes(bytes uint64, isSend bool) {
	if isSend {
		s.send.Add(bytes)
	} else {
		s.recv.Add(bytes)
	}

	s.report(false)
}

func (s *BytesTrackStats) Report() {
	s.report(true)
}

func (p *BytesTrackStats) report(force bool) {
	now := time.Now()
	if !force {
		lr := p.lastStatsReport.Load().(*time.Time)
		if time.Since(*lr) < statsReportInterval {
			return
		}

		if !p.lastStatsReport.CompareAndSwap(lr, &now) {
			return
		}
	} else {
		p.lastStatsReport.Store(&now)
	}

	if recv := p.recv.Swap(0); recv > 0 {
		p.telemetry.TrackStats(StatsKeyForData(livekit.StreamType_UPSTREAM, p.pID, p.trackID), &livekit.AnalyticsStat{
			Streams: []*livekit.AnalyticsStream{
				{PrimaryBytes: recv},
			},
		})
	}

	if send := p.send.Swap(0); send > 0 {
		p.telemetry.TrackStats(StatsKeyForData(livekit.StreamType_DOWNSTREAM, p.pID, p.trackID), &livekit.AnalyticsStat{
			Streams: []*livekit.AnalyticsStream{
				{PrimaryBytes: send},
			},
		})
	}
}

func BytesTrackIDForParticipantID(typ BytesTrackType, participantID livekit.ParticipantID) livekit.TrackID {
	return livekit.TrackID(fmt.Sprintf("%s_%s%s", utils.TrackPrefix, string(typ), participantID))
}
