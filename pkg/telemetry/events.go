package telemetry

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
)

func (t *telemetryService) NotifyEvent(ctx context.Context, event *livekit.WebhookEvent) {
	if t.notifier == nil {
		return
	}

	event.CreatedAt = time.Now().Unix()
	event.Id = utils.NewGuid("EV_")

	t.webhookPool.Submit(func() {
		if err := t.notifier.Notify(ctx, event); err != nil {
			logger.Warnw("failed to notify webhook", err, "event", event.Event)
		}
	})
}

func (t *telemetryService) RoomStarted(ctx context.Context, room *livekit.Room) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event: webhook.EventRoomStarted,
			Room:  room,
		})

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:      livekit.AnalyticsEventType_ROOM_CREATED,
			Timestamp: &timestamppb.Timestamp{Seconds: room.CreationTime},
			Room:      room,
		})
	})
}

func (t *telemetryService) RoomEnded(ctx context.Context, room *livekit.Room) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event: webhook.EventRoomFinished,
			Room:  room,
		})

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:      livekit.AnalyticsEventType_ROOM_ENDED,
			Timestamp: timestamppb.Now(),
			RoomId:    room.Sid,
			Room:      room,
		})
	})
}

func (t *telemetryService) ParticipantJoined(
	ctx context.Context,
	room *livekit.Room,
	participant *livekit.ParticipantInfo,
	clientInfo *livekit.ClientInfo,
	clientMeta *livekit.AnalyticsClientMeta,
) {
	t.enqueue(func() {
		prometheus.IncrementParticipantJoin(1)
		prometheus.AddParticipant()

		t.createWorker(
			ctx,
			livekit.RoomID(room.Sid),
			livekit.RoomName(room.Name),
			livekit.ParticipantID(participant.Sid),
			livekit.ParticipantIdentity(participant.Identity),
		)

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:          livekit.AnalyticsEventType_PARTICIPANT_JOINED,
			Timestamp:     timestamppb.Now(),
			RoomId:        room.Sid,
			ParticipantId: participant.Sid,
			Participant:   participant,
			Room:          room,
			ClientInfo:    clientInfo,
			ClientMeta:    clientMeta,
		})
	})
}

func (t *telemetryService) ParticipantActive(
	ctx context.Context,
	room *livekit.Room,
	participant *livekit.ParticipantInfo,
	clientMeta *livekit.AnalyticsClientMeta,
) {
	t.enqueue(func() {
		// consider participant joined only when they became active
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:       webhook.EventParticipantJoined,
			Room:        room,
			Participant: participant,
		})

		if _, ok := t.getWorker(livekit.ParticipantID(participant.Sid)); !ok {
			t.createWorker(
				ctx,
				livekit.RoomID(room.Sid),
				livekit.RoomName(room.Name),
				livekit.ParticipantID(participant.Sid),
				livekit.ParticipantIdentity(participant.Identity),
			)
		}

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:          livekit.AnalyticsEventType_PARTICIPANT_ACTIVE,
			Timestamp:     timestamppb.Now(),
			RoomId:        room.Sid,
			ParticipantId: participant.Sid,
			Room:          room,
			ClientMeta:    clientMeta,
		})
	})
}

func (t *telemetryService) ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	t.enqueue(func() {
		if worker, ok := t.getWorker(livekit.ParticipantID(participant.Sid)); ok {
			worker.Close()
		}

		prometheus.SubParticipant()

		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:       webhook.EventParticipantLeft,
			Room:        room,
			Participant: participant,
		})

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:          livekit.AnalyticsEventType_PARTICIPANT_LEFT,
			Timestamp:     timestamppb.Now(),
			RoomId:        room.Sid,
			ParticipantId: participant.Sid,
			Participant:   participant,
			Room:          room,
		})
	})
}

func (t *telemetryService) TrackPublished(
	ctx context.Context,
	participantID livekit.ParticipantID,
	identity livekit.ParticipantIdentity,
	track *livekit.TrackInfo,
) {
	t.enqueue(func() {
		prometheus.AddPublishedTrack(track.Type.String())

		roomID, roomName := t.getRoomDetails(participantID)
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event: webhook.EventTrackPublished,
			Room: &livekit.Room{
				Sid:  string(roomID),
				Name: string(roomName),
			},
			Participant: &livekit.ParticipantInfo{
				Sid:      string(participantID),
				Identity: string(identity),
			},
			Track: track,
		})

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:          livekit.AnalyticsEventType_TRACK_PUBLISHED,
			Timestamp:     timestamppb.Now(),
			RoomId:        string(roomID),
			ParticipantId: string(participantID),
			Participant: &livekit.ParticipantInfo{
				Sid:      string(participantID),
				Identity: string(identity),
			},
			Track: track,
			Room:  &livekit.Room{Name: string(roomName)},
		})
	})
}

func (t *telemetryService) TrackPublishedUpdate(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo) {
	t.enqueue(func() {
		roomID, roomName := t.getRoomDetails(participantID)
		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:          livekit.AnalyticsEventType_TRACK_PUBLISHED_UPDATE,
			Timestamp:     timestamppb.Now(),
			RoomId:        string(roomID),
			ParticipantId: string(participantID),
			Track:         track,
			Room:          &livekit.Room{Name: string(roomName)},
		})
	})
}

func (t *telemetryService) TrackMaxSubscribedVideoQuality(
	ctx context.Context,
	participantID livekit.ParticipantID,
	track *livekit.TrackInfo,
	mime string,
	maxQuality livekit.VideoQuality,
) {
	t.enqueue(func() {
		roomID, roomName := t.getRoomDetails(participantID)
		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:                      livekit.AnalyticsEventType_TRACK_MAX_SUBSCRIBED_VIDEO_QUALITY,
			Timestamp:                 timestamppb.Now(),
			RoomId:                    string(roomID),
			ParticipantId:             string(participantID),
			Track:                     track,
			Room:                      &livekit.Room{Name: string(roomName)},
			MaxSubscribedVideoQuality: maxQuality,
			Mime:                      mime,
		})
	})
}

func (t *telemetryService) TrackSubscribed(
	ctx context.Context,
	participantID livekit.ParticipantID,
	track *livekit.TrackInfo,
	publisher *livekit.ParticipantInfo,
) {
	t.enqueue(func() {
		prometheus.AddSubscribedTrack(track.Type.String())

		roomID, roomName := t.getRoomDetails(participantID)
		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:          livekit.AnalyticsEventType_TRACK_SUBSCRIBED,
			Timestamp:     timestamppb.Now(),
			RoomId:        string(roomID),
			ParticipantId: string(participantID),
			Track:         track,
			Room:          &livekit.Room{Name: string(roomName)},
			Publisher:     publisher,
		})
	})
}

func (t *telemetryService) TrackUnsubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo) {
	t.enqueue(func() {
		prometheus.SubSubscribedTrack(track.Type.String())

		roomID, roomName := t.getRoomDetails(participantID)
		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:          livekit.AnalyticsEventType_TRACK_UNSUBSCRIBED,
			Timestamp:     timestamppb.Now(),
			RoomId:        string(roomID),
			ParticipantId: string(participantID),
			TrackId:       track.Sid,
			Room:          &livekit.Room{Name: string(roomName)},
		})
	})
}

func (t *telemetryService) TrackUnpublished(
	ctx context.Context,
	participantID livekit.ParticipantID,
	identity livekit.ParticipantIdentity,
	track *livekit.TrackInfo,
	ssrc uint32,
) {
	t.enqueue(func() {
		roomID, roomName := t.getRoomDetails(participantID)

		prometheus.SubPublishedTrack(track.Type.String())

		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event: webhook.EventTrackUnpublished,
			Room: &livekit.Room{
				Sid:  string(roomID),
				Name: string(roomName),
			},
			Participant: &livekit.ParticipantInfo{
				Sid:      string(participantID),
				Identity: string(identity),
			},
			Track: track,
		})

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:          livekit.AnalyticsEventType_TRACK_UNPUBLISHED,
			Timestamp:     timestamppb.Now(),
			RoomId:        string(roomID),
			ParticipantId: string(participantID),
			TrackId:       track.Sid,
			Room:          &livekit.Room{Name: string(roomName)},
		})
	})
}

func (t *telemetryService) EgressStarted(ctx context.Context, info *livekit.EgressInfo) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:      webhook.EventEgressStarted,
			EgressInfo: info,
		})

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:      livekit.AnalyticsEventType_EGRESS_STARTED,
			Timestamp: timestamppb.Now(),
			EgressId:  info.EgressId,
			RoomId:    info.RoomId,
			Egress:    info,
		})
	})
}

func (t *telemetryService) EgressEnded(ctx context.Context, info *livekit.EgressInfo) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:      webhook.EventEgressEnded,
			EgressInfo: info,
		})

		t.SendEvent(ctx, &livekit.AnalyticsEvent{
			Type:      livekit.AnalyticsEventType_EGRESS_ENDED,
			Timestamp: timestamppb.Now(),
			EgressId:  info.EgressId,
			RoomId:    info.RoomId,
			Egress:    info,
		})
	})
}

func (t *telemetryService) getRoomDetails(participantID livekit.ParticipantID) (livekit.RoomID, livekit.RoomName) {
	if worker, ok := t.getWorker(participantID); ok {
		return worker.roomID, worker.roomName
	}

	return "", ""
}
