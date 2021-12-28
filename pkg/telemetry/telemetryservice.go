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
	NewStatsInterceptorFactory(participantID, identity string) *StatsInterceptorFactory
	AddUpTrack(participantID string, buff *buffer.Buffer)
	OnDownstreamPacket(participantID string, bytes int)
	HandleRTCP(streamType livekit.StreamType, participantID string, pkts []rtcp.Packet)

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

type telemetryService struct {
	internalService TelemetryServiceInternal
}

func NewTelemetryService(notifier webhook.Notifier, analytics AnalyticsService) TelemetryService {
	t := &telemetryService{
		internalService: NewTelemetryServiceInternal(notifier, analytics),
	}

	go t.run()

	return t
}

func (t *telemetryService) run() {
	for {
		select {
		case <-time.After(updateFrequency):
			t.internalService.SendAnalytics()
		}
	}
}

func (t *telemetryService) AddUpTrack(participantID string, buff *buffer.Buffer) {
	t.internalService.AddUpTrack(participantID, buff)
}

func (t *telemetryService) OnDownstreamPacket(participantID string, bytes int) {
	t.internalService.OnDownstreamPacket(participantID, bytes)
}

func (t *telemetryService) HandleRTCP(streamType livekit.StreamType, participantID string, pkts []rtcp.Packet) {
	t.internalService.HandleRTCP(streamType, participantID, pkts)
}

func (t *telemetryService) RoomStarted(ctx context.Context, room *livekit.Room) {
	t.internalService.RoomStarted(ctx, room)
}

func (t *telemetryService) RoomEnded(ctx context.Context, room *livekit.Room) {
	t.internalService.RoomEnded(ctx, room)
}

func (t *telemetryService) ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientInfo *livekit.ClientInfo) {
	t.internalService.ParticipantJoined(ctx, room, participant, clientInfo)
}

func (t *telemetryService) ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	t.internalService.ParticipantLeft(ctx, room, participant)
}

func (t *telemetryService) TrackPublished(ctx context.Context, participantID string, track *livekit.TrackInfo) {
	t.internalService.TrackPublished(ctx, participantID, track)
}

func (t *telemetryService) TrackUnpublished(ctx context.Context, participantID string, track *livekit.TrackInfo, ssrc uint32) {
	t.internalService.TrackUnpublished(ctx, participantID, track, ssrc)
}

func (t *telemetryService) TrackSubscribed(ctx context.Context, participantID string, track *livekit.TrackInfo) {
	t.internalService.TrackSubscribed(ctx, participantID, track)
}

func (t *telemetryService) TrackUnsubscribed(ctx context.Context, participantID string, track *livekit.TrackInfo) {
	t.internalService.TrackUnsubscribed(ctx, participantID, track)
}

func (t *telemetryService) RecordingStarted(ctx context.Context, ri *livekit.RecordingInfo) {
	t.internalService.RecordingStarted(ctx, ri)
}

func (t *telemetryService) RecordingEnded(ctx context.Context, ri *livekit.RecordingInfo) {
	t.internalService.RecordingEnded(ctx, ri)
}

func (t *telemetryService) NewStatsInterceptorFactory(participantID, identity string) *StatsInterceptorFactory {
	return t.internalService.NewStatsInterceptorFactory(participantID, identity)
}
