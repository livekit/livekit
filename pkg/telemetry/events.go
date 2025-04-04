// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package telemetry

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/protocol/webhook"
)

func (t *telemetryService) NotifyEvent(ctx context.Context, event *livekit.WebhookEvent) {
	if t.notifier == nil {
		return
	}

	event.CreatedAt = time.Now().Unix()
	event.Id = guid.New("EV_")

	if err := t.notifier.QueueNotify(ctx, event); err != nil {
		logger.Warnw("failed to notify webhook", err, "event", event.Event)
	}
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
	shouldSendEvent bool,
) {
	t.enqueue(func() {
		_, found := t.getOrCreateWorker(
			ctx,
			livekit.RoomID(room.Sid),
			livekit.RoomName(room.Name),
			livekit.ParticipantID(participant.Sid),
			livekit.ParticipantIdentity(participant.Identity),
		)
		if !found {
			prometheus.IncrementParticipantRtcConnected(1)
			prometheus.AddParticipant()
		}

		if shouldSendEvent {
			ev := newParticipantEvent(livekit.AnalyticsEventType_PARTICIPANT_JOINED, room, participant)
			ev.ClientInfo = clientInfo
			ev.ClientMeta = clientMeta
			t.SendEvent(ctx, ev)
		}
	})
}

func (t *telemetryService) ParticipantActive(
	ctx context.Context,
	room *livekit.Room,
	participant *livekit.ParticipantInfo,
	clientMeta *livekit.AnalyticsClientMeta,
	isMigration bool,
) {
	t.enqueue(func() {
		if !isMigration {
			// consider participant joined only when they became active
			t.NotifyEvent(ctx, &livekit.WebhookEvent{
				Event:       webhook.EventParticipantJoined,
				Room:        room,
				Participant: participant,
			})
		}

		worker, found := t.getOrCreateWorker(
			ctx,
			livekit.RoomID(room.Sid),
			livekit.RoomName(room.Name),
			livekit.ParticipantID(participant.Sid),
			livekit.ParticipantIdentity(participant.Identity),
		)
		if !found {
			// need to also account for participant count
			prometheus.AddParticipant()
		}
		worker.SetConnected()

		ev := newParticipantEvent(livekit.AnalyticsEventType_PARTICIPANT_ACTIVE, room, participant)
		ev.ClientMeta = clientMeta
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) ParticipantResumed(
	ctx context.Context,
	room *livekit.Room,
	participant *livekit.ParticipantInfo,
	nodeID livekit.NodeID,
	reason livekit.ReconnectReason,
) {
	t.enqueue(func() {
		// create a worker if needed.
		//
		// Signalling channel stats collector and media channel stats collector could both call
		// ParticipantJoined and ParticipantLeft.
		//
		// On a resume, the signalling channel collector would call `ParticipantLeft` which would close
		// the corresponding participant's stats worker.
		//
		// So, on a successful resume, create the worker if needed.
		_, found := t.getOrCreateWorker(
			ctx,
			livekit.RoomID(room.Sid),
			livekit.RoomName(room.Name),
			livekit.ParticipantID(participant.Sid),
			livekit.ParticipantIdentity(participant.Identity),
		)
		if !found {
			prometheus.AddParticipant()
		}

		ev := newParticipantEvent(livekit.AnalyticsEventType_PARTICIPANT_RESUMED, room, participant)
		ev.ClientMeta = &livekit.AnalyticsClientMeta{
			Node:            string(nodeID),
			ReconnectReason: reason,
		}
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) ParticipantLeft(ctx context.Context,
	room *livekit.Room,
	participant *livekit.ParticipantInfo,
	shouldSendEvent bool,
) {
	t.enqueue(func() {
		isConnected := false
		if worker, ok := t.getWorker(livekit.ParticipantID(participant.Sid)); ok {
			isConnected = worker.IsConnected()
			if worker.Close() {
				prometheus.SubParticipant()
			}
		}

		if isConnected && shouldSendEvent {
			t.NotifyEvent(ctx, &livekit.WebhookEvent{
				Event:       webhook.EventParticipantLeft,
				Room:        room,
				Participant: participant,
			})

			t.SendEvent(ctx, newParticipantEvent(livekit.AnalyticsEventType_PARTICIPANT_LEFT, room, participant))
		}
	})
}

func (t *telemetryService) TrackPublishRequested(
	ctx context.Context,
	participantID livekit.ParticipantID,
	identity livekit.ParticipantIdentity,
	track *livekit.TrackInfo,
) {
	t.enqueue(func() {
		prometheus.AddPublishAttempt(track.Type.String(), track.Source.String(), track.MimeType)
		room := t.getRoomDetails(participantID)
		ev := newTrackEvent(livekit.AnalyticsEventType_TRACK_PUBLISH_REQUESTED, room, participantID, track)
		if ev.Participant != nil {
			ev.Participant.Identity = string(identity)
		}
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) TrackPublished(
	ctx context.Context,
	participantID livekit.ParticipantID,
	identity livekit.ParticipantIdentity,
	track *livekit.TrackInfo,
) {
	t.enqueue(func() {
		prometheus.AddPublishedTrack(track.Type.String(), track.Source.String(), track.MimeType)
		prometheus.AddPublishSuccess(track.Type.String(), track.Source.String(), track.MimeType)

		room := t.getRoomDetails(participantID)
		participant := &livekit.ParticipantInfo{
			Sid:      string(participantID),
			Identity: string(identity),
		}
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:       webhook.EventTrackPublished,
			Room:        room,
			Participant: participant,
			Track:       track,
		})

		ev := newTrackEvent(livekit.AnalyticsEventType_TRACK_PUBLISHED, room, participantID, track)
		ev.Participant = participant
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) TrackPublishedUpdate(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo) {
	t.enqueue(func() {
		room := t.getRoomDetails(participantID)
		t.SendEvent(ctx, newTrackEvent(livekit.AnalyticsEventType_TRACK_PUBLISHED_UPDATE, room, participantID, track))
	})
}

func (t *telemetryService) TrackMaxSubscribedVideoQuality(
	ctx context.Context,
	participantID livekit.ParticipantID,
	track *livekit.TrackInfo,
	mime mime.MimeType,
	maxQuality livekit.VideoQuality,
) {
	t.enqueue(func() {
		room := t.getRoomDetails(participantID)
		ev := newTrackEvent(livekit.AnalyticsEventType_TRACK_MAX_SUBSCRIBED_VIDEO_QUALITY, room, participantID, track)
		ev.MaxSubscribedVideoQuality = maxQuality
		ev.Mime = mime.String()
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) TrackSubscribeRequested(
	ctx context.Context,
	participantID livekit.ParticipantID,
	track *livekit.TrackInfo,
) {
	t.enqueue(func() {
		prometheus.RecordTrackSubscribeAttempt()

		room := t.getRoomDetails(participantID)
		ev := newTrackEvent(livekit.AnalyticsEventType_TRACK_SUBSCRIBE_REQUESTED, room, participantID, track)
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) TrackSubscribed(
	ctx context.Context,
	participantID livekit.ParticipantID,
	track *livekit.TrackInfo,
	publisher *livekit.ParticipantInfo,
	shouldSendEvent bool,
) {
	t.enqueue(func() {
		prometheus.RecordTrackSubscribeSuccess(track.Type.String(), track.Source.String(), track.MimeType)

		if !shouldSendEvent {
			return
		}

		room := t.getRoomDetails(participantID)
		ev := newTrackEvent(livekit.AnalyticsEventType_TRACK_SUBSCRIBED, room, participantID, track)
		ev.Publisher = publisher
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) TrackSubscribeFailed(
	ctx context.Context,
	participantID livekit.ParticipantID,
	trackID livekit.TrackID,
	err error,
	isUserError bool,
) {
	t.enqueue(func() {
		prometheus.RecordTrackSubscribeFailure(err, isUserError)

		room := t.getRoomDetails(participantID)
		ev := newTrackEvent(livekit.AnalyticsEventType_TRACK_SUBSCRIBE_FAILED, room, participantID, &livekit.TrackInfo{
			Sid: string(trackID),
		})
		ev.Error = err.Error()
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) TrackUnsubscribed(
	ctx context.Context,
	participantID livekit.ParticipantID,
	track *livekit.TrackInfo,
	shouldSendEvent bool,
) {
	t.enqueue(func() {
		prometheus.RecordTrackUnsubscribed(track.Type.String(), track.Source.String(), track.MimeType)

		if shouldSendEvent {
			room := t.getRoomDetails(participantID)
			t.SendEvent(ctx, newTrackEvent(livekit.AnalyticsEventType_TRACK_UNSUBSCRIBED, room, participantID, track))
		}
	})
}

func (t *telemetryService) TrackUnpublished(
	ctx context.Context,
	participantID livekit.ParticipantID,
	identity livekit.ParticipantIdentity,
	track *livekit.TrackInfo,
	shouldSendEvent bool,
) {
	t.enqueue(func() {
		prometheus.SubPublishedTrack(track.Type.String(), track.Source.String(), track.MimeType)
		if !shouldSendEvent {
			return
		}

		room := t.getRoomDetails(participantID)
		participant := &livekit.ParticipantInfo{
			Sid:      string(participantID),
			Identity: string(identity),
		}
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:       webhook.EventTrackUnpublished,
			Room:        room,
			Participant: participant,
			Track:       track,
		})

		t.SendEvent(ctx, newTrackEvent(livekit.AnalyticsEventType_TRACK_UNPUBLISHED, room, participantID, track))
	})
}

func (t *telemetryService) TrackMuted(
	ctx context.Context,
	participantID livekit.ParticipantID,
	track *livekit.TrackInfo,
) {
	t.enqueue(func() {
		room := t.getRoomDetails(participantID)
		t.SendEvent(ctx, newTrackEvent(livekit.AnalyticsEventType_TRACK_MUTED, room, participantID, track))
	})
}

func (t *telemetryService) TrackUnmuted(
	ctx context.Context,
	participantID livekit.ParticipantID,
	track *livekit.TrackInfo,
) {
	t.enqueue(func() {
		room := t.getRoomDetails(participantID)
		t.SendEvent(ctx, newTrackEvent(livekit.AnalyticsEventType_TRACK_UNMUTED, room, participantID, track))
	})
}

func (t *telemetryService) TrackPublishRTPStats(
	ctx context.Context,
	participantID livekit.ParticipantID,
	trackID livekit.TrackID,
	mimeType mime.MimeType,
	layer int,
	stats *livekit.RTPStats,
) {
	t.enqueue(func() {
		room := t.getRoomDetails(participantID)
		ev := newRoomEvent(livekit.AnalyticsEventType_TRACK_PUBLISH_STATS, room)
		ev.ParticipantId = string(participantID)
		ev.TrackId = string(trackID)
		ev.Mime = mimeType.String()
		ev.VideoLayer = int32(layer)
		ev.RtpStats = stats
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) TrackSubscribeRTPStats(
	ctx context.Context,
	participantID livekit.ParticipantID,
	trackID livekit.TrackID,
	mimeType mime.MimeType,
	stats *livekit.RTPStats,
) {
	t.enqueue(func() {
		room := t.getRoomDetails(participantID)
		ev := newRoomEvent(livekit.AnalyticsEventType_TRACK_SUBSCRIBE_STATS, room)
		ev.ParticipantId = string(participantID)
		ev.TrackId = string(trackID)
		ev.Mime = mimeType.String()
		ev.RtpStats = stats
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) EgressStarted(ctx context.Context, info *livekit.EgressInfo) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:      webhook.EventEgressStarted,
			EgressInfo: info,
		})

		t.SendEvent(ctx, newEgressEvent(livekit.AnalyticsEventType_EGRESS_STARTED, info))
	})
}

func (t *telemetryService) EgressUpdated(ctx context.Context, info *livekit.EgressInfo) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:      webhook.EventEgressUpdated,
			EgressInfo: info,
		})
		t.SendEvent(ctx, newEgressEvent(livekit.AnalyticsEventType_EGRESS_UPDATED, info))
	})
}

func (t *telemetryService) EgressEnded(ctx context.Context, info *livekit.EgressInfo) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:      webhook.EventEgressEnded,
			EgressInfo: info,
		})

		t.SendEvent(ctx, newEgressEvent(livekit.AnalyticsEventType_EGRESS_ENDED, info))
	})
}

func (t *telemetryService) IngressCreated(ctx context.Context, info *livekit.IngressInfo) {
	t.enqueue(func() {
		t.SendEvent(ctx, newIngressEvent(livekit.AnalyticsEventType_INGRESS_CREATED, info))
	})
}

func (t *telemetryService) IngressDeleted(ctx context.Context, info *livekit.IngressInfo) {
	t.enqueue(func() {
		t.SendEvent(ctx, newIngressEvent(livekit.AnalyticsEventType_INGRESS_DELETED, info))
	})
}

func (t *telemetryService) IngressStarted(ctx context.Context, info *livekit.IngressInfo) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:       webhook.EventIngressStarted,
			IngressInfo: info,
		})

		t.SendEvent(ctx, newIngressEvent(livekit.AnalyticsEventType_INGRESS_STARTED, info))
	})
}

func (t *telemetryService) IngressUpdated(ctx context.Context, info *livekit.IngressInfo) {
	t.enqueue(func() {
		t.SendEvent(ctx, newIngressEvent(livekit.AnalyticsEventType_INGRESS_UPDATED, info))
	})
}

func (t *telemetryService) IngressEnded(ctx context.Context, info *livekit.IngressInfo) {
	t.enqueue(func() {
		t.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event:       webhook.EventIngressEnded,
			IngressInfo: info,
		})

		t.SendEvent(ctx, newIngressEvent(livekit.AnalyticsEventType_INGRESS_ENDED, info))
	})
}

func (t *telemetryService) Report(ctx context.Context, reportInfo *livekit.ReportInfo) {
	t.enqueue(func() {
		ev := &livekit.AnalyticsEvent{
			Type:      livekit.AnalyticsEventType_REPORT,
			Timestamp: timestamppb.Now(),
			Report:    reportInfo,
		}
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) APICall(ctx context.Context, apiCallInfo *livekit.APICallInfo) {
	t.enqueue(func() {
		ev := &livekit.AnalyticsEvent{
			Type:      livekit.AnalyticsEventType_API_CALL,
			Timestamp: timestamppb.Now(),
			ApiCall:   apiCallInfo,
		}
		t.SendEvent(ctx, ev)
	})
}

func (t *telemetryService) Webhook(ctx context.Context, webhookInfo *livekit.WebhookInfo) {
	t.enqueue(func() {
		ev := &livekit.AnalyticsEvent{
			Type:      livekit.AnalyticsEventType_WEBHOOK,
			Timestamp: timestamppb.Now(),
			Webhook:   webhookInfo,
		}
		t.SendEvent(ctx, ev)
	})
}

// returns a livekit.Room with only name and sid filled out
// returns nil if room is not found
func (t *telemetryService) getRoomDetails(participantID livekit.ParticipantID) *livekit.Room {
	if worker, ok := t.getWorker(participantID); ok {
		return &livekit.Room{
			Sid:  string(worker.roomID),
			Name: string(worker.roomName),
		}
	}

	return nil
}

func newRoomEvent(event livekit.AnalyticsEventType, room *livekit.Room) *livekit.AnalyticsEvent {
	ev := &livekit.AnalyticsEvent{
		Type:      event,
		Timestamp: timestamppb.Now(),
	}
	if room != nil {
		ev.Room = room
		ev.RoomId = room.Sid
	}
	return ev
}

func newParticipantEvent(event livekit.AnalyticsEventType, room *livekit.Room, participant *livekit.ParticipantInfo) *livekit.AnalyticsEvent {
	ev := newRoomEvent(event, room)
	if participant != nil {
		ev.ParticipantId = participant.Sid
		ev.Participant = participant
	}
	return ev
}

func newTrackEvent(event livekit.AnalyticsEventType, room *livekit.Room, participantID livekit.ParticipantID, track *livekit.TrackInfo) *livekit.AnalyticsEvent {
	ev := newParticipantEvent(event, room, &livekit.ParticipantInfo{
		Sid: string(participantID),
	})
	if track != nil {
		ev.TrackId = track.Sid
		ev.Track = track
	}
	return ev
}

func newEgressEvent(event livekit.AnalyticsEventType, egress *livekit.EgressInfo) *livekit.AnalyticsEvent {
	return &livekit.AnalyticsEvent{
		Type:      event,
		Timestamp: timestamppb.Now(),
		EgressId:  egress.EgressId,
		RoomId:    egress.RoomId,
		Egress:    egress,
	}
}

func newIngressEvent(event livekit.AnalyticsEventType, ingress *livekit.IngressInfo) *livekit.AnalyticsEvent {
	return &livekit.AnalyticsEvent{
		Type:      event,
		Timestamp: timestamppb.Now(),
		IngressId: ingress.IngressId,
		Ingress:   ingress,
	}
}
