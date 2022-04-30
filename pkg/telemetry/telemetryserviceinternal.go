package telemetry

import (
	"context"

	"github.com/gammazero/workerpool"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const maxWebhookWorkers = 50

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
		webhookPool: workerpool.New(maxWebhookWorkers),
		workers:     make(map[livekit.ParticipantID]*StatsWorker),
		analytics:   analytics,
	}
}

func (t *telemetryServiceInternal) TrackStats(streamType livekit.StreamType, participantID livekit.ParticipantID, trackID livekit.TrackID, stat *livekit.AnalyticsStat) {
	direction := prometheus.Incoming
	if streamType == livekit.StreamType_DOWNSTREAM {
		direction = prometheus.Outgoing
	}

	nacks := uint32(0)
	plis := uint32(0)
	firs := uint32(0)
	for _, stream := range stat.Streams {
		nacks += stream.Nacks
		plis += stream.Plis
		firs += stream.Firs
	}
	prometheus.IncrementRTCP(direction, nacks, plis, firs)

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

		packets := uint32(0)
		bytes := uint64(0)
		for _, stream := range stat.Streams {
			packets += stream.PrimaryPackets + stream.RetransmitPackets + stream.PaddingPackets
			bytes += stream.PrimaryBytes + stream.RetransmitBytes + stream.PaddingBytes
		}
		prometheus.IncrementPackets(direction, uint64(packets))
		prometheus.IncrementBytes(direction, bytes)
	}

	t.analytics.SendStats(ctx, stats)
}

func (t *telemetryServiceInternal) SendAnalytics() {
	for _, worker := range t.workers {
		worker.Update()
	}
}
