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
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/webhook"
	"golang.org/x/exp/maps"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 . TelemetryService
type TelemetryService interface {
	// TrackStats is called periodically for each track in both directions (published/subscribed)
	TrackStats(key StatsKey, stat *livekit.AnalyticsStat)

	// events
	RoomStarted(ctx context.Context, room *livekit.Room)
	RoomEnded(ctx context.Context, room *livekit.Room)
	// ParticipantJoined - a participant establishes signal connection to a room
	ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientInfo *livekit.ClientInfo, clientMeta *livekit.AnalyticsClientMeta, shouldSendEvent bool)
	// ParticipantActive - a participant establishes media connection
	ParticipantActive(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientMeta *livekit.AnalyticsClientMeta, isMigration bool)
	// ParticipantResumed - there has been an ICE restart or connection resume attempt, and we've received their signal connection
	ParticipantResumed(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, nodeID livekit.NodeID, reason livekit.ReconnectReason)
	// ParticipantLeft - the participant leaves the room, only sent if ParticipantActive has been called before
	ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, shouldSendEvent bool)
	// TrackPublishRequested - a publication attempt has been received
	TrackPublishRequested(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo)
	// TrackPublished - a publication attempt has been successful
	TrackPublished(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo)
	// TrackUnpublished - a participant unpublished a track
	TrackUnpublished(ctx context.Context, participantID livekit.ParticipantID, identity livekit.ParticipantIdentity, track *livekit.TrackInfo, shouldSendEvent bool)
	// TrackSubscribeRequested - a participant requested to subscribe to a track
	TrackSubscribeRequested(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo)
	// TrackSubscribed - a participant subscribed to a track successfully
	TrackSubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, publisher *livekit.ParticipantInfo, shouldSendEvent bool)
	// TrackUnsubscribed - a participant unsubscribed from a track successfully
	TrackUnsubscribed(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, shouldSendEvent bool)
	// TrackSubscribeFailed - failure to subscribe to a track
	TrackSubscribeFailed(ctx context.Context, participantID livekit.ParticipantID, trackID livekit.TrackID, err error, isUserError bool)
	// TrackMuted - the publisher has muted the Track
	TrackMuted(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo)
	// TrackUnmuted - the publisher has muted the Track
	TrackUnmuted(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo)
	// TrackPublishedUpdate - track metadata has been updated
	TrackPublishedUpdate(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo)
	// TrackMaxSubscribedVideoQuality - publisher is notified of the max quality subscribers desire
	TrackMaxSubscribedVideoQuality(ctx context.Context, participantID livekit.ParticipantID, track *livekit.TrackInfo, mime string, maxQuality livekit.VideoQuality)
	TrackPublishRTPStats(ctx context.Context, participantID livekit.ParticipantID, trackID livekit.TrackID, mimeType string, layer int, stats *livekit.RTPStats)
	TrackSubscribeRTPStats(ctx context.Context, participantID livekit.ParticipantID, trackID livekit.TrackID, mimeType string, stats *livekit.RTPStats)
	EgressStarted(ctx context.Context, info *livekit.EgressInfo)
	EgressUpdated(ctx context.Context, info *livekit.EgressInfo)
	EgressEnded(ctx context.Context, info *livekit.EgressInfo)
	IngressCreated(ctx context.Context, info *livekit.IngressInfo)
	IngressDeleted(ctx context.Context, info *livekit.IngressInfo)
	IngressStarted(ctx context.Context, info *livekit.IngressInfo)
	IngressUpdated(ctx context.Context, info *livekit.IngressInfo)
	IngressEnded(ctx context.Context, info *livekit.IngressInfo)
	LocalRoomState(ctx context.Context, info *livekit.AnalyticsNodeRooms)

	// helpers
	AnalyticsService
	NotifyEvent(ctx context.Context, event *livekit.WebhookEvent)
	FlushStats()
}

const (
	workerCleanupWait = 3 * time.Minute
	jobsQueueMinSize  = 2048
)

type telemetryService struct {
	AnalyticsService

	notifier  webhook.QueuedNotifier
	jobsQueue *utils.OpsQueue

	lock          sync.RWMutex
	workers       map[livekit.ParticipantID]*StatsWorker
	workersShadow []*StatsWorker
}

func NewTelemetryService(notifier webhook.QueuedNotifier, analytics AnalyticsService) TelemetryService {
	t := &telemetryService{
		AnalyticsService: analytics,

		notifier:  notifier,
		jobsQueue: utils.NewOpsQueue("telemetry", jobsQueueMinSize, true),
		workers:   make(map[livekit.ParticipantID]*StatsWorker),
	}

	t.jobsQueue.Start()
	go t.run()

	return t
}

func (t *telemetryService) FlushStats() {
	t.lock.RLock()
	workersShadow := t.workersShadow
	t.lock.RUnlock()

	for _, worker := range workersShadow {
		worker.Flush()
	}
}

func (t *telemetryService) run() {
	ticker := time.NewTicker(config.TelemetryStatsUpdateInterval)
	defer ticker.Stop()

	cleanupTicker := time.NewTicker(time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ticker.C:
			t.FlushStats()
		case <-cleanupTicker.C:
			t.cleanupWorkers()
		}
	}
}

func (t *telemetryService) enqueue(op func()) {
	t.jobsQueue.Enqueue(op)
}

func (t *telemetryService) getWorker(participantID livekit.ParticipantID) (worker *StatsWorker, ok bool) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	worker, ok = t.workers[participantID]
	return
}

func (t *telemetryService) createWorker(ctx context.Context,
	roomID livekit.RoomID,
	roomName livekit.RoomName,
	participantID livekit.ParticipantID,
	participantIdentity livekit.ParticipantIdentity,
) *StatsWorker {
	worker := newStatsWorker(
		ctx,
		t,
		roomID,
		roomName,
		participantID,
		participantIdentity,
	)

	t.lock.Lock()
	t.workers[participantID] = worker
	t.workersShadow = maps.Values(t.workers)
	t.lock.Unlock()
	return worker
}

func (t *telemetryService) cleanupWorkers() {
	t.lock.RLock()
	workersShadow := t.workersShadow
	t.lock.RUnlock()

	toReap := make([]livekit.ParticipantID, 0, len(workersShadow))
	for _, worker := range t.workers {
		closedAt := worker.ClosedAt()
		if !closedAt.IsZero() && time.Since(closedAt) > workerCleanupWait {
			worker.Flush()

			toReap = append(toReap, worker.ParticipantID())
		}
	}

	if len(toReap) == 0 {
		return
	}

	t.lock.Lock()
	logger.Debugw("reaping analytics worker for participants", "pID", toReap)
	for _, pID := range toReap {
		delete(t.workers, pID)
	}
	t.workersShadow = maps.Values(t.workers)
	t.lock.Unlock()
}

func (t *telemetryService) LocalRoomState(ctx context.Context, info *livekit.AnalyticsNodeRooms) {
	t.enqueue(func() {
		t.SendNodeRoomStates(ctx, info)
	})
}
