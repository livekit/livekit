package telemetry

import (
	"context"
	"sync"

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
	workersMu  sync.RWMutex
	workers    []*StatsWorker
	workersIdx map[livekit.ParticipantID]int

	analytics AnalyticsService
}

func NewTelemetryServiceInternal(notifier webhook.Notifier, analytics AnalyticsService) TelemetryServiceInternal {
	return &telemetryServiceInternal{
		notifier:    notifier,
		webhookPool: workerpool.New(maxWebhookWorkers),
		workersIdx:  make(map[livekit.ParticipantID]int),
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
	packets := uint32(0)
	bytes := uint64(0)
	for _, stream := range stat.Streams {
		nacks += stream.Nacks
		plis += stream.Plis
		firs += stream.Firs
		packets += stream.PrimaryPackets + stream.RetransmitPackets + stream.PaddingPackets
		bytes += stream.PrimaryBytes + stream.RetransmitBytes + stream.PaddingBytes
	}
	prometheus.IncrementRTCP(direction, nacks, plis, firs)
	prometheus.IncrementPackets(direction, uint64(packets))
	prometheus.IncrementBytes(direction, bytes)

	if w := t.getStatsWorker(participantID); w != nil {
		w.OnTrackStat(trackID, streamType, stat)
	}
}

func (t *telemetryServiceInternal) Report(ctx context.Context, stats []*livekit.AnalyticsStat) {
	t.analytics.SendStats(ctx, stats)
}

func (t *telemetryServiceInternal) SendAnalytics() {
	t.workersMu.RLock()
	workers := t.workers
	t.workersMu.RUnlock()

	for _, worker := range workers {
		if worker != nil {
			worker.Update()
		}
	}
}

func (t *telemetryServiceInternal) getStatsWorker(participantID livekit.ParticipantID) *StatsWorker {
	t.workersMu.RLock()
	defer t.workersMu.RUnlock()

	if idx, ok := t.workersIdx[participantID]; ok {
		return t.workers[idx]
	}

	return nil
}
