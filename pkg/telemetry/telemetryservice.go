package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/gammazero/workerpool"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/webhook"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 . TelemetryService
type TelemetryService interface {
	// stats
	TrackStats(streamType livekit.StreamType, participantID livekit.ParticipantID, trackID livekit.TrackID, stat *livekit.AnalyticsStat)

	// events
	RoomStarted(ctx context.Context, room *livekit.Room)
	RoomEnded(ctx context.Context, room *livekit.Room)
	ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientInfo *livekit.ClientInfo, clientMeta *livekit.AnalyticsClientMeta)
	ParticipantActive(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientMeta *livekit.AnalyticsClientMeta)
	ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo)
	TrackPublished(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo)
	TrackUnpublished(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo, ssrc uint32)
	TrackSubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, publisher *livekit.ParticipantInfo)
	TrackUnsubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo)
	TrackPublishedUpdate(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo)
	TrackMaxSubscribedVideoQuality(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, mime string, maxQuality livekit.VideoQuality)
	EgressStarted(ctx context.Context, info *livekit.EgressInfo)
	EgressEnded(ctx context.Context, info *livekit.EgressInfo)

	// helpers
	AnalyticsService
	NotifyEvent(ctx context.Context, event *livekit.WebhookEvent)
	FlushStats()
}

const (
	maxWebhookWorkers  = 50
	workerCleanupWait  = 3 * time.Minute
	jobQueueBufferSize = 10000
)

type telemetryService struct {
	AnalyticsService

	notifier    webhook.Notifier
	webhookPool *workerpool.WorkerPool
	jobsChan    chan func()

	// one worker per participant
	workersMu  sync.RWMutex
	workers    []*StatsWorker
	workersIdx map[livekit.ParticipantID]int
}

func NewTelemetryService(notifier webhook.Notifier, analytics AnalyticsService) TelemetryService {
	t := &telemetryService{
		AnalyticsService: analytics,

		notifier:    notifier,
		webhookPool: workerpool.New(maxWebhookWorkers),
		jobsChan:    make(chan func(), jobQueueBufferSize),

		workersIdx: make(map[livekit.ParticipantID]int),
	}

	go t.run()

	return t
}

func (t *telemetryService) run() {
	ticker := time.NewTicker(config.StatsUpdateInterval)
	defer ticker.Stop()

	cleanupTicker := time.NewTicker(time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ticker.C:
			t.FlushStats()
		case <-cleanupTicker.C:
			t.cleanupWorkers()
		case op := <-t.jobsChan:
			op()
		}
	}
}

func (t *telemetryService) enqueue(op func()) {
	select {
	case t.jobsChan <- op:
	// success
	default:
		logger.Warnw("telemetry queue full", nil)
	}
}

func (t *telemetryService) cleanupWorkers() {
	t.workersMu.RLock()
	workers := t.workers
	t.workersMu.RUnlock()

	for _, worker := range workers {
		if worker == nil {
			continue
		}

		closedAt := worker.ClosedAt()
		if !closedAt.IsZero() && time.Since(closedAt) > workerCleanupWait {
			pID := worker.ParticipantID()
			logger.Debugw("reaping analytics worker for participant", "pID", pID)
			t.workersMu.Lock()
			if idx, ok := t.workersIdx[pID]; ok {
				delete(t.workersIdx, pID)
				t.workers[idx] = nil
			}
			t.workersMu.Unlock()
		}
	}
}
