package telemetry

import (
	"context"
	"time"

	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/webhook"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

func (t *TelemetryService) RoomStarted(ctx context.Context, room *livekit.Room) {
	prometheus.RoomStarted()

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRoomStarted,
		Room:  room,
	})

	// TODO: analytics service
}

func (t *TelemetryService) RoomEnded(ctx context.Context, room *livekit.Room) {
	prometheus.RoomEnded(time.Unix(room.CreationTime, 0))

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRoomFinished,
		Room:  room,
	})

	// TODO: analytics service
}

func (t *TelemetryService) ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	prometheus.AddParticipant()

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:       webhook.EventParticipantJoined,
		Room:        room,
		Participant: participant,
	})

	// TODO: analytics service
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

	// TODO: analytics service
}

func (t *TelemetryService) TrackPublished(participantID, identity string, track *livekit.TrackInfo, buff *buffer.Buffer) {
	t.Lock()
	if t.workers[participantID] == nil {
		t.workers[participantID] = NewStatsWorker(func(diff *buffer.Stats) {
			t.HandleIncomingRTP(participantID, identity, diff)
		})
	}
	t.workers[participantID].AddBuffer(buff)
	t.Unlock()

	prometheus.AddPublishedTrack(track.Type.String())

	// TODO: analytics service
}

func (t *TelemetryService) TrackUnpublished(participantID, identity string, track *livekit.TrackInfo, ssrc uint32) {
	t.RLock()
	if w := t.workers[participantID]; w != nil {
		w.RemoveBuffer(ssrc)
	}
	t.RUnlock()

	prometheus.SubPublishedTrack(track.Type.String())

	// TODO: analytics service
}

func (t *TelemetryService) TrackSubscribed(participantID, identity string, track *livekit.TrackInfo) {
	prometheus.AddSubscribedTrack(track.Type.String())

	// TODO: analytics service
}

func (t *TelemetryService) TrackUnsubscribed(participantID, identity string, track *livekit.TrackInfo) {
	prometheus.SubSubscribedTrack(track.Type.String())

	// TODO: analytics service
}

func (t *TelemetryService) RecordingStarted(ctx context.Context, recordingID string, req *livekit.StartRecordingRequest) {
	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRecordingStarted,
		RecordingInfo: &livekit.RecordingInfo{
			Id:      recordingID,
			Request: req,
		},
	})

	// TODO: analytics service
}

func (t *TelemetryService) RecordingEnded(res *livekit.RecordingResult) {
	t.notifyEvent(context.Background(), &livekit.WebhookEvent{
		Event:           webhook.EventRecordingFinished,
		RecordingResult: res,
	})

	// TODO: analytics service
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
