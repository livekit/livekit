package telemetry

import (
	"context"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

func (t *telemetryServiceInternal) RoomStarted(ctx context.Context, room *livekit.Room) {
	prometheus.RoomStarted()

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRoomStarted,
		Room:  room,
	})

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:      livekit.AnalyticsEventType_ROOM_CREATED,
		Timestamp: &timestamppb.Timestamp{Seconds: room.CreationTime},
		Room:      room,
	})
}

func (t *telemetryServiceInternal) RoomEnded(ctx context.Context, room *livekit.Room) {
	prometheus.RoomEnded(time.Unix(room.CreationTime, 0))

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event: webhook.EventRoomFinished,
		Room:  room,
	})

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:      livekit.AnalyticsEventType_ROOM_ENDED,
		Timestamp: timestamppb.Now(),
		RoomId:    room.Sid,
		Room:      room,
	})
}

func (t *telemetryServiceInternal) ParticipantJoined(ctx context.Context, room *livekit.Room,
	participant *livekit.ParticipantInfo, clientInfo *livekit.ClientInfo, clientMeta *livekit.AnalyticsClientMeta) {
	t.workers[livekit.ParticipantID(participant.Sid)] = newStatsWorker(
		ctx,
		t,
		livekit.RoomID(room.Sid),
		livekit.RoomName(room.Name),
		livekit.ParticipantID(participant.Sid),
		livekit.ParticipantIdentity(participant.Identity),
	)

	prometheus.AddParticipant()

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_PARTICIPANT_JOINED,
		Timestamp:     timestamppb.Now(),
		RoomId:        room.Sid,
		ParticipantId: participant.Sid,
		Participant:   participant,
		Room:          room,
		ClientInfo:    clientInfo,
		ClientMeta:    clientMeta,
	})
}

func (t *telemetryServiceInternal) ParticipantActive(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientMeta *livekit.AnalyticsClientMeta) {
	// consider participant joined only when they became active
	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:       webhook.EventParticipantJoined,
		Room:        room,
		Participant: participant,
	})

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_PARTICIPANT_ACTIVE,
		Timestamp:     timestamppb.Now(),
		RoomId:        room.Sid,
		ParticipantId: participant.Sid,
		Room:          room,
		ClientMeta:    clientMeta,
	})
}

func (t *telemetryServiceInternal) ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo) {
	if w := t.workers[livekit.ParticipantID(participant.Sid)]; w != nil {
		w.Close()
		delete(t.workers, livekit.ParticipantID(participant.Sid))
	}

	prometheus.SubParticipant()

	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:       webhook.EventParticipantLeft,
		Room:        room,
		Participant: participant,
	})

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_PARTICIPANT_LEFT,
		Timestamp:     timestamppb.Now(),
		RoomId:        room.Sid,
		ParticipantId: participant.Sid,
		Participant:   participant,
		Room:          room,
	})
}

func (t *telemetryServiceInternal) TrackPublished(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo) {
	prometheus.AddPublishedTrack(track.Type.String())

	roomID, roomName := t.getRoomDetails(participantID)
	t.notifyEvent(ctx, &livekit.WebhookEvent{
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

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_PUBLISHED,
		Timestamp:     timestamppb.Now(),
		RoomId:        string(roomID),
		ParticipantId: string(participantID),
		Track:         track,
		Room:          &livekit.Room{Name: string(roomName)},
	})
}

func (t *telemetryServiceInternal) TrackPublishedUpdate(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo) {
	roomID, roomName := t.getRoomDetails(participantID)
	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_PUBLISHED_UPDATE,
		Timestamp:     timestamppb.Now(),
		RoomId:        string(roomID),
		ParticipantId: string(participantID),
		Track:         track,
		Room:          &livekit.Room{Name: string(roomName)},
	})
}

func (t *telemetryServiceInternal) TrackMaxSubscribedVideoQuality(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo,
	maxQuality livekit.VideoQuality) {

	roomID, roomName := t.getRoomDetails(participantID)
	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:                      livekit.AnalyticsEventType_TRACK_MAX_SUBSCRIBED_VIDEO_QUALITY,
		Timestamp:                 timestamppb.Now(),
		RoomId:                    string(roomID),
		ParticipantId:             string(participantID),
		Track:                     track,
		Room:                      &livekit.Room{Name: string(roomName)},
		MaxSubscribedVideoQuality: maxQuality,
	})
}

func (t *telemetryServiceInternal) TrackUnpublished(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo, ssrc uint32) {
	roomID := livekit.RoomID("")
	roomName := livekit.RoomName("")
	w := t.workers[participantID]
	if w != nil {
		roomID = w.roomID
		roomName = w.roomName
		w.RemoveStats(livekit.TrackID(track.GetSid()))
	}

	prometheus.SubPublishedTrack(track.Type.String())

	t.notifyEvent(ctx, &livekit.WebhookEvent{
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

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_UNPUBLISHED,
		Timestamp:     timestamppb.Now(),
		RoomId:        string(roomID),
		ParticipantId: string(participantID),
		TrackId:       track.Sid,
		Room:          &livekit.Room{Name: string(roomName)},
	})
}

func (t *telemetryServiceInternal) TrackSubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo,
	publisher *livekit.ParticipantInfo) {
	prometheus.AddSubscribedTrack(track.Type.String())

	roomID, roomName := t.getRoomDetails(participantID)
	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_SUBSCRIBED,
		Timestamp:     timestamppb.Now(),
		RoomId:        string(roomID),
		ParticipantId: string(participantID),
		Track:         track,
		Room:          &livekit.Room{Name: string(roomName)},
		Publisher:     publisher,
	})
}

func (t *telemetryServiceInternal) TrackUnsubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo) {
	prometheus.SubSubscribedTrack(track.Type.String())

	roomID, roomName := t.getRoomDetails(participantID)
	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:          livekit.AnalyticsEventType_TRACK_UNSUBSCRIBED,
		Timestamp:     timestamppb.Now(),
		RoomId:        string(roomID),
		ParticipantId: string(participantID),
		TrackId:       track.Sid,
		Room:          &livekit.Room{Name: string(roomName)},
	})
}

func (t *telemetryServiceInternal) RecordingStarted(ctx context.Context, ri *livekit.RecordingInfo) {
	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:         webhook.EventRecordingStarted,
		RecordingInfo: ri,
	})

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:        livekit.AnalyticsEventType_RECORDING_STARTED,
		Timestamp:   timestamppb.Now(),
		RecordingId: ri.Id,
		Room:        &livekit.Room{Name: ri.RoomName},
	})
}

func (t *telemetryServiceInternal) RecordingEnded(ctx context.Context, ri *livekit.RecordingInfo) {
	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:         webhook.EventRecordingFinished,
		RecordingInfo: ri,
	})

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:        livekit.AnalyticsEventType_RECORDING_ENDED,
		Timestamp:   timestamppb.Now(),
		RecordingId: ri.Id,
		Room:        &livekit.Room{Name: ri.RoomName},
	})
}

func (t *telemetryServiceInternal) getRoomDetails(participantID livekit.ParticipantID) (livekit.RoomID, livekit.RoomName) {
	w := t.workers[participantID]
	if w != nil {
		return w.roomID, w.roomName
	}
	return "", ""
}

func (t *telemetryServiceInternal) notifyEvent(ctx context.Context, event *livekit.WebhookEvent) {
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

func (t *telemetryServiceInternal) EgressStarted(ctx context.Context, info *livekit.EgressInfo) {
	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:      webhook.EventEgressStarted,
		EgressInfo: info,
	})

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:      livekit.AnalyticsEventType_EGRESS_STARTED,
		Timestamp: timestamppb.Now(),
		EgressId:  info.EgressId,
		RoomId:    info.RoomId,
	})
}

func (t *telemetryServiceInternal) EgressEnded(ctx context.Context, info *livekit.EgressInfo) {
	t.notifyEvent(ctx, &livekit.WebhookEvent{
		Event:      webhook.EventEgressEnded,
		EgressInfo: info,
	})

	t.analytics.SendEvent(ctx, &livekit.AnalyticsEvent{
		Type:      livekit.AnalyticsEventType_EGRESS_ENDED,
		Timestamp: timestamppb.Now(),
		EgressId:  info.EgressId,
		RoomId:    info.RoomId,
	})
}
