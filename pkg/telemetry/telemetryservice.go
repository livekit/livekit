package telemetry

import (
	"context"
	"time"

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
}

type telemetryService struct {
	internalService TelemetryServiceInternal
	jobsChan        chan func()
}

// queue should be sufficiently large to avoid blocking
const jobQueueBufferSize = 10000

func NewTelemetryService(notifier webhook.Notifier, analytics AnalyticsService) TelemetryService {
	t := &telemetryService{
		internalService: NewTelemetryServiceInternal(notifier, analytics),
		jobsChan:        make(chan func(), jobQueueBufferSize),
	}

	go t.run()

	return t
}

func (t *telemetryService) run() {
	ticker := time.NewTicker(config.StatsUpdateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.internalService.SendAnalytics()
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

func (t *telemetryService) TrackStats(streamType livekit.StreamType, participantID livekit.ParticipantID, trackID livekit.TrackID, stats *livekit.AnalyticsStat) {
	t.enqueue(func() {
		t.internalService.TrackStats(streamType, participantID, trackID, stats)
	})
}

func (t *telemetryService) RoomStarted(ctx context.Context, room *livekit.Room) {
	t.enqueue(func() {
		t.internalService.RoomStarted(ctx, room)
	})
}

func (t *telemetryService) RoomEnded(ctx context.Context, room *livekit.Room) {
	t.enqueue(func() {
		t.internalService.RoomEnded(ctx, room)
	})
}

func (t *telemetryService) ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo,
	clientInfo *livekit.ClientInfo, clientMeta *livekit.AnalyticsClientMeta) {
	t.enqueue(func() {
		t.internalService.ParticipantJoined(ctx, room, participant, clientInfo, clientMeta)
	})
}

func (t *telemetryService) ParticipantActive(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientMeta *livekit.AnalyticsClientMeta) {
	t.enqueue(func() {
		t.internalService.ParticipantActive(ctx, room, participant, clientMeta)
	})
}

func (t *telemetryService) ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	t.enqueue(func() {
		t.internalService.ParticipantLeft(ctx, room, participant)
	})
}

func (t *telemetryService) TrackPublished(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo) {
	t.enqueue(func() {
		t.internalService.TrackPublished(ctx, participantID, identity, track)
	})
}

func (t *telemetryService) TrackUnpublished(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo, ssrc uint32) {
	t.enqueue(func() {
		t.internalService.TrackUnpublished(ctx, participantID, identity, track, ssrc)
	})
}

func (t *telemetryService) TrackSubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, publisher *livekit.ParticipantInfo) {
	t.enqueue(func() {
		t.internalService.TrackSubscribed(ctx, participantID, track, publisher)
	})
}

func (t *telemetryService) TrackUnsubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo) {
	t.enqueue(func() {
		t.internalService.TrackUnsubscribed(ctx, participantID, track)
	})
}

func (t *telemetryService) TrackPublishedUpdate(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo) {
	t.enqueue(func() {
		t.internalService.TrackPublishedUpdate(ctx, participantID, track)
	})
}

func (t *telemetryService) TrackMaxSubscribedVideoQuality(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, mime string, maxQuality livekit.VideoQuality) {
	t.enqueue(func() {
		t.internalService.TrackMaxSubscribedVideoQuality(ctx, participantID, track, mime, maxQuality)
	})
}

func (t *telemetryService) EgressStarted(ctx context.Context, info *livekit.EgressInfo) {
	t.enqueue(func() {
		t.internalService.EgressStarted(ctx, info)
	})
}

func (t *telemetryService) EgressEnded(ctx context.Context, info *livekit.EgressInfo) {
	t.enqueue(func() {
		t.internalService.EgressEnded(ctx, info)
	})
}
