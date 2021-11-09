package telemetry

import (
	"context"
	"time"

	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/webhook"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

func (s *TelemetryService) RoomStarted(ctx context.Context, room *livekit.Room) {
	s.pool.Submit(prometheus.RoomStarted)

	s.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRoomStarted,
		Room:  room,
	})

	// TODO: analytics service
}

func (s *TelemetryService) RoomEnded(ctx context.Context, room *livekit.Room) {
	s.pool.Submit(func() {
		prometheus.RoomEnded(time.Unix(room.CreationTime, 0))
	})

	s.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRoomFinished,
		Room:  room,
	})

	// TODO: analytics service
}

func (s *TelemetryService) ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	s.pool.Submit(prometheus.AddParticipant)

	s.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:       webhook.EventParticipantJoined,
		Room:        room,
		Participant: participant,
	})

	// TODO: analytics service
}

func (s *TelemetryService) ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	s.pool.Submit(prometheus.SubParticipant)

	s.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:       webhook.EventParticipantLeft,
		Room:        room,
		Participant: participant,
	})

	// TODO: analytics service
}

func (s *TelemetryService) PublishedTrack(SID, identity string, track *livekit.TrackInfo) {
	s.pool.Submit(func() {
		prometheus.AddPublishedTrack(track.Type.String())
	})

	// TODO: analytics service
}

func (s *TelemetryService) UnpublishedTrack(SID, identity string, track *livekit.TrackInfo) {
	s.pool.Submit(func() {
		prometheus.SubPublishedTrack(track.Type.String())
	})

	// TODO: analytics service
}

func (s *TelemetryService) SubscribedTrack(SID, identity string, track *livekit.TrackInfo) {
	s.pool.Submit(func() {
		prometheus.AddSubscribedTrack(track.Type.String())
	})

	// TODO: analytics service
}

func (s *TelemetryService) UnsubscribedTrack(SID, identity string, track *livekit.TrackInfo) {
	s.pool.Submit(func() {
		prometheus.SubSubscribedTrack(track.Type.String())
	})

	// TODO: analytics service
}

func (s *TelemetryService) RecordingStarted(ctx context.Context, recordingID string, req *livekit.StartRecordingRequest) {
	logger.Debugw("recording started", "recordingID", recordingID)

	s.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRecordingStarted,
		RecordingInfo: &livekit.RecordingInfo{
			Id:      recordingID,
			Request: req,
		},
	})

	// TODO: analytics service
}

func (s *TelemetryService) RecordingEnded(res *livekit.RecordingResult) {
	// log results
	values := []interface{}{"recordingID", res.Id}
	if res.Error != "" {
		values = append(values, "error", res.Error)
	} else {
		values = append(values, "duration", time.Duration(res.Duration*1e9))
		if res.DownloadUrl != "" {
			values = append(values, "url", res.DownloadUrl)
		}
	}
	logger.Debugw("recording ended", values...)

	s.notifyEvent(context.Background(), &livekit.WebhookEvent{
		Event:           webhook.EventRecordingFinished,
		RecordingResult: res,
	})

	// TODO: analytics service
}

func (s *TelemetryService) notifyEvent(ctx context.Context, event *livekit.WebhookEvent) {
	if s.notifier == nil {
		return
	}

	s.pool.Submit(func() {
		if err := s.notifier.Notify(ctx, event); err != nil {
			logger.Warnw("failed to notify webhook", err, "event", event.Event)
		}
	})
}
