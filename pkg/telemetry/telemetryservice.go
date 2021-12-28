package telemetry

import (
	"context"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"github.com/pion/rtcp"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const updateFrequency = time.Second * 10

type TelemetryService interface {
	// stats
	AddUpTrack(participantID string, trackID string, buff *buffer.Buffer)
	OnDownstreamPacket(participantID string, trackID string, bytes int)
	HandleRTCP(streamType livekit.StreamType, participantID string, trackID string, pkts []rtcp.Packet)

	// events
	RoomStarted(ctx context.Context, room *livekit.Room)
	RoomEnded(ctx context.Context, room *livekit.Room)
	ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientInfo *livekit.ClientInfo)
	ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo)
	TrackPublished(ctx context.Context, participantID string, track *livekit.TrackInfo)
	TrackUnpublished(ctx context.Context, participantID string, track *livekit.TrackInfo, ssrc uint32)
	TrackSubscribed(ctx context.Context, participantID string, track *livekit.TrackInfo)
	TrackUnsubscribed(ctx context.Context, participantID string, track *livekit.TrackInfo)
	RecordingStarted(ctx context.Context, ri *livekit.RecordingInfo)
	RecordingEnded(ctx context.Context, ri *livekit.RecordingInfo)
}

type doWorkFunc func()

type telemetryService struct {
	internalService TelemetryServiceInternal
	jobQueue        chan doWorkFunc
}

const jobQueueBufferSize = 100

func NewTelemetryService(notifier webhook.Notifier, analytics AnalyticsService) TelemetryService {
	t := &telemetryService{
		internalService: NewTelemetryServiceInternal(notifier, analytics),
		jobQueue:        make(chan doWorkFunc, jobQueueBufferSize),
	}

	go t.run()

	return t
}

func (t *telemetryService) run() {

	ticker := time.NewTicker(updateFrequency)
	for {
		select {
		case <-ticker.C:
			t.internalService.SendAnalytics()
		case job, ok := <-t.jobQueue:
			if ok {
				job()
			}
		}
	}
}

func (t *telemetryService) AddUpTrack(participantID string, trackID string, buff *buffer.Buffer) {
	t.jobQueue <- func() {
		t.internalService.AddUpTrack(participantID, trackID, buff)
	}
}

func (t *telemetryService) OnDownstreamPacket(participantID string, trackID string, bytes int) {
	t.jobQueue <- func() {
		t.internalService.OnDownstreamPacket(participantID, trackID, bytes)
	}
}

func (t *telemetryService) HandleRTCP(streamType livekit.StreamType, participantID string, trackID string, pkts []rtcp.Packet) {
	t.jobQueue <- func() {
		t.internalService.HandleRTCP(streamType, participantID, trackID, pkts)
	}
}

func (t *telemetryService) RoomStarted(ctx context.Context, room *livekit.Room) {
	t.jobQueue <- func() {
		t.internalService.RoomStarted(ctx, room)
	}
}

func (t *telemetryService) RoomEnded(ctx context.Context, room *livekit.Room) {
	t.jobQueue <- func() {
		t.internalService.RoomEnded(ctx, room)
	}
}

func (t *telemetryService) ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientInfo *livekit.ClientInfo) {
	t.jobQueue <- func() {
		t.internalService.ParticipantJoined(ctx, room, participant, clientInfo)
	}
}

func (t *telemetryService) ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	t.jobQueue <- func() {
		t.internalService.ParticipantLeft(ctx, room, participant)
	}
}

func (t *telemetryService) TrackPublished(ctx context.Context, participantID string, track *livekit.TrackInfo) {
	t.jobQueue <- func() {
		t.internalService.TrackPublished(ctx, participantID, track)
	}
}

func (t *telemetryService) TrackUnpublished(ctx context.Context, participantID string, track *livekit.TrackInfo, ssrc uint32) {
	t.jobQueue <- func() {
		t.internalService.TrackUnpublished(ctx, participantID, track, ssrc)
	}
}

func (t *telemetryService) TrackSubscribed(ctx context.Context, participantID string, track *livekit.TrackInfo) {
	t.jobQueue <- func() {
		t.internalService.TrackSubscribed(ctx, participantID, track)
	}
}

func (t *telemetryService) TrackUnsubscribed(ctx context.Context, participantID string, track *livekit.TrackInfo) {
	t.jobQueue <- func() {
		t.internalService.TrackUnsubscribed(ctx, participantID, track)
	}
}

func (t *telemetryService) RecordingStarted(ctx context.Context, ri *livekit.RecordingInfo) {
	t.jobQueue <- func() {
		t.internalService.RecordingStarted(ctx, ri)
	}
}

func (t *telemetryService) RecordingEnded(ctx context.Context, ri *livekit.RecordingInfo) {
	t.jobQueue <- func() {
		t.internalService.RecordingEnded(ctx, ri)
	}
}
