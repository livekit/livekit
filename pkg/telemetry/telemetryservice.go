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

	lock    sync.RWMutex
	workers map[livekit.ParticipantID]*StatsWorker
}

func NewTelemetryService(notifier webhook.Notifier, analytics AnalyticsService) TelemetryService {
	t := &telemetryService{
		AnalyticsService: analytics,

		notifier:    notifier,
		webhookPool: workerpool.New(maxWebhookWorkers),
		jobsChan:    make(chan func(), jobQueueBufferSize),
		workers:     make(map[livekit.ParticipantID]*StatsWorker),
	}

	go t.run()

	return t
}

func (t *telemetryService) FlushStats() {
	t.lock.Lock()
	defer t.lock.Unlock()

	for _, worker := range t.workers {
		worker.Flush()
	}
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

func (t *telemetryService) getWorker(participantID livekit.ParticipantID) (worker *StatsWorker, ok bool) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	worker, ok = t.workers[participantID]
	return
}

func (t *telemetryService) createWorker(ctx context.Context, roomID livekit.RoomID, roomName livekit.RoomName,
	participantID livekit.ParticipantID, participantIdentity livekit.ParticipantIdentity) {
	worker := newStatsWorker(
		ctx,
		t,
		roomID,
		roomName,
		participantID,
		participantIdentity,
	)

	t.lock.Lock()
	t.workers[participantID] = worker
	t.lock.Unlock()
}

func (t *telemetryService) cleanupWorkers() {
	t.lock.Lock()
	defer t.lock.Unlock()

	for participantID, worker := range t.workers {
		closedAt := worker.ClosedAt()
		if !closedAt.IsZero() && time.Since(closedAt) > workerCleanupWait {
			logger.Debugw("reaping analytics worker for participant", "pID", participantID)
			delete(t.workers, participantID)
		}
	}
}
