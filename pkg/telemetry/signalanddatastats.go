package telemetry

import (
	"fmt"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
)

const statsReportInterval = 10 * time.Second

// stats for signal and data channel
type SignalAndDataStats struct {
	trackID         livekit.TrackID
	pID             livekit.ParticipantID
	send, recv      atomic.Uint64
	lastStatsReport atomic.Value // *time.Time
	telemetry       TelemetryService
}

func NewBytesTrackStats(datatrack bool, pID livekit.ParticipantID, telemetry TelemetryService) *SignalAndDataStats {
	var trackID livekit.TrackID
	if datatrack {
		trackID = dataTrackIDForParticipantID(pID)
	} else {
		trackID = signalTrackIDForRoomID(pID)
	}

	s := &SignalAndDataStats{
		trackID:   trackID,
		pID:       pID,
		telemetry: telemetry,
	}
	now := time.Now()
	s.lastStatsReport.Store(&now)
	return s
}

func (s *SignalAndDataStats) AddBytes(bytes uint64, isSend bool) {
	if isSend {
		s.send.Add(bytes)
	} else {
		s.recv.Add(bytes)
	}

	s.report(false)
}

func (s *SignalAndDataStats) Report() {
	s.report(true)
}

func (p *SignalAndDataStats) report(force bool) {
	now := time.Now()
	if !force {
		lr := p.lastStatsReport.Load().(*time.Time)
		if lr.Add(statsReportInterval).After(now) {
			return
		}

		if !p.lastStatsReport.CompareAndSwap(lr, &now) {
			return
		}
	} else {
		p.lastStatsReport.Store(&now)
	}

	if recv := p.recv.Swap(0); recv > 0 {
		p.telemetry.TrackStats(livekit.StreamType_UPSTREAM, p.pID, p.trackID, &livekit.AnalyticsStat{
			Streams: []*livekit.AnalyticsStream{
				{PrimaryBytes: recv},
			},
		})
	}

	if send := p.send.Swap(0); send > 0 {
		p.telemetry.TrackStats(livekit.StreamType_DOWNSTREAM, p.pID, p.trackID, &livekit.AnalyticsStat{
			Streams: []*livekit.AnalyticsStream{
				{PrimaryBytes: send},
			},
		})
	}
}

func dataTrackIDForParticipantID(participantID livekit.ParticipantID) livekit.TrackID {
	return livekit.TrackID(fmt.Sprintf("%s_DT%s", utils.TrackPrefix, participantID))
}

func signalTrackIDForRoomID(participantID livekit.ParticipantID) livekit.TrackID {
	return livekit.TrackID(fmt.Sprintf("%s_SG%s", utils.TrackPrefix, participantID))
}
