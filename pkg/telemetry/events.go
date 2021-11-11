package telemetry

import (
	"context"
	"time"

	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/webhook"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

func (t *TelemetryService) RoomStarted(ctx context.Context, room *livekit.Room) {
	prometheus.RoomStarted()

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRoomStarted,
		Room:  room,
	})

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:      livekit.AnalyticsEventType_ROOM_CREATED,
		Timestamp: &timestamppb.Timestamp{Seconds: room.CreationTime},
		Room:      room,
	})
}

func (t *TelemetryService) RoomEnded(ctx context.Context, room *livekit.Room) {
	prometheus.RoomEnded(time.Unix(room.CreationTime, 0))

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRoomFinished,
		Room:  room,
	})

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:      livekit.AnalyticsEventType_ROOM_ENDED,
		Timestamp: timestamppb.Now(),
		RoomSid:   room.Sid,
	})
}

func (t *TelemetryService) ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	t.Lock()
	t.workers[participant.Sid] = NewStatsWorker(func(diff *ParticipantStats) {
		t.UpdateStats(room.Sid, participant.Sid, diff)
	})
	t.Unlock()

	prometheus.AddParticipant()

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:       webhook.EventParticipantJoined,
		Room:        room,
		Participant: participant,
	})

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:        livekit.AnalyticsEventType_PARTICIPANT_JOINED,
		Timestamp:   timestamppb.Now(),
		Participant: participant,
	})
}

func (t *TelemetryService) ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	t.Lock()
	if w := t.workers[participant.Sid]; w != nil {
		w.Close()
		delete(t.workers, participant.Sid)
	}
	t.Unlock()

	prometheus.SubParticipant()

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:       webhook.EventParticipantLeft,
		Room:        room,
		Participant: participant,
	})

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_PARTICIPANT_LEFT,
		Timestamp:     timestamppb.Now(),
		ParticipantId: participant.Sid,
	})
}

func (t *TelemetryService) TrackPublished(participantID string, track *livekit.TrackInfo, buff *buffer.Buffer) {
	t.RLock()
	if w := t.workers[participantID]; w != nil {
		w.AddBuffer(buff)
	}
	t.RUnlock()

	prometheus.AddPublishedTrack(track.Type.String())

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_PUBLISHED,
		Timestamp:     timestamppb.Now(),
		ParticipantId: participantID,
		Track:         track,
	})
}

func (t *TelemetryService) TrackUnpublished(participantID string, track *livekit.TrackInfo, ssrc uint32) {
	t.RLock()
	if w := t.workers[participantID]; w != nil {
		w.RemoveBuffer(ssrc)
	}
	t.RUnlock()

	prometheus.SubPublishedTrack(track.Type.String())

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_UNPUBLISHED,
		Timestamp:     timestamppb.Now(),
		ParticipantId: participantID,
		TrackId:       track.Sid,
	})
}

func (t *TelemetryService) TrackSubscribed(participantID string, track *livekit.TrackInfo) {
	prometheus.AddSubscribedTrack(track.Type.String())

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_SUBSCRIBED,
		Timestamp:     timestamppb.Now(),
		ParticipantId: participantID,
		TrackId:       track.Sid,
	})
}

func (t *TelemetryService) TrackUnsubscribed(participantID string, track *livekit.TrackInfo) {
	prometheus.SubSubscribedTrack(track.Type.String())

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_UNSUBSCRIBED,
		Timestamp:     timestamppb.Now(),
		ParticipantId: participantID,
		TrackId:       track.Sid,
	})
}

func (t *TelemetryService) RecordingStarted(ctx context.Context, recordingID string, req *livekit.StartRecordingRequest) {
	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRecordingStarted,
		RecordingInfo: &livekit.RecordingInfo{
			Id:      recordingID,
			Request: req,
		},
	})

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:        livekit.AnalyticsEventType_RECORDING_STARTED,
		Timestamp:   timestamppb.Now(),
		RecordingId: recordingID,
	})
}

func (t *TelemetryService) RecordingEnded(res *livekit.RecordingResult) {
	t.notifyEvent(context.Background(), &livekit.WebhookEvent{
		Event:           webhook.EventRecordingFinished,
		RecordingResult: res,
	})

	t.sendEvent(&livekit.AnalyticsEvent{
		Type:        livekit.AnalyticsEventType_RECORDING_ENDED,
		Timestamp:   timestamppb.Now(),
		RecordingId: res.Id,
	})
}

func (t *TelemetryService) notifyEvent(ctx context.Context, event *livekit.WebhookEvent) {
	if t.notifier == nil {
		return
	}

	t.webhookPool.Submit(func() {
		if err := t.notifier.Notify(ctx, event); err != nil {
			logger.Warnw("failed to notify webhook", err, "event", event.Event)
		}
	})
}

func (t *TelemetryService) sendEvent(event *livekit.AnalyticsEvent) {
	if t.analyticsEnabled {
		if err := t.events.Send(&livekit.AnalyticsEvents{
			Events: []*livekit.AnalyticsEvent{event},
		}); err != nil {
			logger.Errorw("failed to send event", err, "eventType", event.Type.String())
		}
	}
}
