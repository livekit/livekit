package telemetry

import (
	"context"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/webhook"
)

const updateFrequency = time.Second * 10

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
	TrackMaxSubscribedVideoQuality(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, maxQuality livekit.VideoQuality)
	RecordingStarted(ctx context.Context, ri *livekit.RecordingInfo)
	RecordingEnded(ctx context.Context, ri *livekit.RecordingInfo)
	EgressStarted(ctx context.Context, info *livekit.EgressInfo)
	EgressEnded(ctx context.Context, info *livekit.EgressInfo)
}

type doWorkFunc func()

type telemetryService struct {
	internalService TelemetryServiceInternal
	jobQueue        chan doWorkFunc
}

// queue should be sufficiently large to avoid blocking
const jobQueueBufferSize = 10000

func NewTelemetryService(notifier webhook.Notifier, analytics AnalyticsService) TelemetryService {
	t := &telemetryService{
		internalService: NewTelemetryServiceInternal(notifier, analytics),
		jobQueue:        make(chan doWorkFunc, jobQueueBufferSize),
	}

	go t.run()

	return t
}

func (t *telemetryService) run() {
	lastUpdatedAt := time.Now()
	ticker := time.NewTicker(updateFrequency)
	for {
		select {
		case <-ticker.C:
			lastUpdatedAt = t.internalService.SendAnalytics()
		case job, ok := <-t.jobQueue:
			if ok {
				job()
			}
			// check if we missed sending analytics every updateFrequency seconds
			if time.Now().After(lastUpdatedAt.Add(updateFrequency)) {
				lastUpdatedAt = t.internalService.SendAnalytics()
			}
		}
	}
}

func (t *telemetryService) enqueue(f func()) {
	select {
	case t.jobQueue <- f:
		return
	default:
		logger.Warnw("telemetry queue full, dropping message", nil)
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

func (t *telemetryService) RecordingStarted(ctx context.Context, ri *livekit.RecordingInfo) {
	t.enqueue(func() {
		t.internalService.RecordingStarted(ctx, ri)
	})
}

func (t *telemetryService) RecordingEnded(ctx context.Context, ri *livekit.RecordingInfo) {
	t.enqueue(func() {
		t.internalService.RecordingEnded(ctx, ri)
	})
}

func (t *telemetryService) TrackPublishedUpdate(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo) {
	t.enqueue(func() {
		t.internalService.TrackPublishedUpdate(ctx, participantID, track)
	})
}

func (t *telemetryService) TrackMaxSubscribedVideoQuality(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, maxQuality livekit.VideoQuality) {
	t.enqueue(func() {
		t.internalService.TrackMaxSubscribedVideoQuality(ctx, participantID, track, maxQuality)
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
