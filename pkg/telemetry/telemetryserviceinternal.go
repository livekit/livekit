package telemetry

import (
	"context"

	"github.com/gammazero/workerpool"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
)

type TelemetryServiceInternal interface {
	TelemetryService
	SendAnalytics()
}

type TelemetryReporter interface {
	Report(ctx context.Context, stats []*livekit.AnalyticsStat)
}

type telemetryServiceInternal struct {
	notifier    webhook.Notifier
	webhookPool *workerpool.WorkerPool

	// one worker per participant
	workers map[livekit.ParticipantID]*StatsWorker

	analytics AnalyticsService
}

func NewTelemetryServiceInternal(notifier webhook.Notifier, analytics AnalyticsService) TelemetryServiceInternal {
	return &telemetryServiceInternal{
		notifier:    notifier,
		webhookPool: workerpool.New(1),
		workers:     make(map[livekit.ParticipantID]*StatsWorker),
		analytics:   analytics,
	}
}

func (t *telemetryServiceInternal) TrackStats(streamType livekit.StreamType, participantID livekit.ParticipantID, trackID livekit.TrackID, stat *livekit.AnalyticsStat) {

	direction := prometheus.Incoming
	if streamType == livekit.StreamType_DOWNSTREAM {
		direction = prometheus.Outgoing
	}

	totalNACKs := uint32(0)
	totalPLIs := uint32(0)
	totalFIRs := uint32(0)
	for _, stream := range stat.Streams {
		totalNACKs += stream.TotalNacks
		totalPLIs += stream.TotalPlis
		totalFIRs += stream.TotalFirs
	}
	prometheus.IncrementRTCP(direction, totalNACKs, totalPLIs, totalFIRs)

	w := t.workers[participantID]
	if w != nil {
		w.OnTrackStat(trackID, streamType, stat)
	}
}

func (t *telemetryServiceInternal) Report(ctx context.Context, stats []*livekit.AnalyticsStat) {
	for _, stat := range stats {
		if len(stat.Streams) == 0 {
			continue
		}

		direction := prometheus.Incoming
		if stat.Kind == livekit.StreamType_DOWNSTREAM {
			direction = prometheus.Outgoing
		}

		totalPackets := uint32(0)
		totalBytes := uint64(0)
		for _, stream := range stat.Streams {
			totalPackets += (stream.TotalPrimaryPackets + stream.TotalRetransmitPackets + stream.TotalPaddingPackets)
			totalBytes += (stream.TotalPrimaryBytes + stream.TotalRetransmitBytes + stream.TotalPaddingBytes)
		}
		prometheus.IncrementPackets(direction, uint64(totalPackets))
		prometheus.IncrementBytes(direction, totalBytes)
	}

	t.analytics.SendStats(ctx, stats)
}

func (t *telemetryServiceInternal) SendAnalytics() {
	for _, worker := range t.workers {
		worker.Update()
	}
}
