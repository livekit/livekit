// Copyright 2026 OpenVidu
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

//go:build integration

package analytics

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/livekit/livekit-server/openvidu/analytics/schema"
	"github.com/livekit/livekit-server/openvidu/analytics/store"
	"github.com/livekit/livekit-server/openvidu/analytics/testutil"
	"github.com/livekit/livekit-server/openvidu/openviduconfig"
)

type testEnv struct {
	t        *testing.T
	db       *mongo.Database
	settings openviduconfig.AnalyticsSettings
	p        *processor
}

var nonAlphanumeric = regexp.MustCompile(`[^A-Za-z0-9]+`)

func newEnv(t *testing.T, client *mongo.Client, configure ...func(*openviduconfig.AnalyticsConfig)) *testEnv {
	t.Helper()
	conf := openviduconfig.AnalyticsConfig{Enabled: true, MongoUrl: "mongodb://test"}
	for _, c := range configure {
		c(&conf)
	}
	settings, _, err := conf.Settings()
	require.NoError(t, err)

	name := nonAlphanumeric.ReplaceAllString(t.Name(), "_")
	if len(name) > 60 {
		name = name[len(name)-60:]
	}
	db := client.Database(name)
	require.NoError(t, db.Drop(context.Background()))
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	_, err = schema.Initialize(context.Background(), db, schema.InitOptions{
		ValidationAction: "error", Retention: retentionOf(settings), StatsDetail: settings.StatsDetail, WriterVersion: "test",
	})
	require.NoError(t, err)

	return &testEnv{t: t, db: db, settings: settings, p: &processor{
		store: store.New(db), opts: projectionOptions(settings), cache: newRefCache(),
		onWriteErrors: func(err *store.WriteError) { t.Errorf("write rejected: %v", err) },
	}}
}

func (e *testEnv) apply(batch []*livekit.AnalyticsEvent, stats ...[]*livekit.AnalyticsStat) {
	e.t.Helper()
	entries := make([]*eventEntry, len(batch))
	for i, event := range batch {
		entries[i] = &eventEntry{event: event}
	}
	var statsEntries []*statsEntry
	for _, flush := range stats {
		statsEntries = append(statsEntries, &statsEntry{stats: flush})
	}
	require.NoError(e.t, e.p.apply(context.Background(), entries, statsEntries))
}

func find[T any](e *testEnv, collection string, id string) T {
	e.t.Helper()
	var document T
	require.NoError(e.t, e.db.Collection(collection).FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&document),
		"%s %s", collection, id)
	return document
}

func count(e *testEnv, collection string, filter bson.D) int64 {
	e.t.Helper()
	n, err := e.db.Collection(collection).CountDocuments(context.Background(), filter)
	require.NoError(e.t, err)
	return n
}

// snapshot reads every document of the given collections without the time-series _id.
func snapshot(e *testEnv, collections ...string) map[string][]bson.M {
	e.t.Helper()
	result := map[string][]bson.M{}
	for _, collection := range collections {
		cursor, err := e.db.Collection(collection).Find(context.Background(), bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
		require.NoError(e.t, err)
		var documents []bson.M
		require.NoError(e.t, cursor.All(context.Background(), &documents))
		result[collection] = documents
	}
	return result
}

var allCollections = []string{
	schema.CollectionRooms, schema.CollectionParticipants, schema.CollectionTracks, schema.CollectionTrackSubscriptions,
	schema.CollectionEgresses, schema.CollectionIngresses, schema.CollectionIngressSessions, schema.CollectionEvents,
	schema.CollectionAnalytics5Min, schema.CollectionAnalyticsHourly, schema.CollectionAnalyticsDaily,
}

func TestWriter(t *testing.T) {
	client := testutil.StartMongo(t)

	t.Run("room lifecycle", func(t *testing.T) {
		env := newEnv(t, client)
		env.p.opts.MediaNode = testMediaNode
		var ev events
		created := at("0s")
		a := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("5s"))
		b := participantInfo("PA_b", "bob", livekit.ParticipantInfo_STANDARD, at("6s"))
		c := participantInfo("PA_c", "carol", livekit.ParticipantInfo_STANDARD, at("7s"))
		env.apply([]*livekit.AnalyticsEvent{
			ev.roomCreated("RM_1", "Daily", created),
			ev.joined("RM_1", a, at("5s")), ev.active("RM_1", a, at("6s")),
			ev.joined("RM_1", b, at("6s")), ev.active("RM_1", b, at("7s")),
			ev.joined("RM_1", c, at("7s")), ev.active("RM_1", c, at("8s")),
			ev.left("RM_1", a, at("1m"), livekit.DisconnectReason_CLIENT_INITIATED),
			ev.roomEnded("RM_1", "Daily", created, at("2m"), livekit.RoomEndReason_ROOM_END_API_DELETE),
		})

		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.Equal(t, "closed", room.Status)
		require.Equal(t, "API_DELETE", room.EndReason)
		require.Equal(t, "event", room.ClosedBy)
		require.EqualValues(t, 3, room.Participants.Sessions)
		require.EqualValues(t, 0, room.Participants.Active)
		require.EqualValues(t, 3, room.Participants.PeakActive)
		require.EqualValues(t, 3, room.Participants.Unique)
		require.Equal(t, int64(120_000), *room.DurationMs)
		require.True(t, room.EndedAt.Add(90*24*time.Hour).Equal(*room.ExpiresAt))
		require.Empty(t, room.Config.Metadata, "metadata is not stored by default")

		hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z")
		require.EqualValues(t, 1, *hourly.Rooms.Created)
		require.EqualValues(t, 1, *hourly.Rooms.Ended)
		require.EqualValues(t, 3, *hourly.Rooms.ParticipantsSum)
		require.Equal(t, int64(120_000), *hourly.Rooms.DurationMsSum)
		require.EqualValues(t, 3, *hourly.Participants.Joined)
		require.EqualValues(t, 3, hourly.Participants.BySDK["JS"])
		require.EqualValues(t, 3, hourly.Participants.ByOS["windows"])
		require.EqualValues(t, 3, hourly.Participants.ByBrowser["chrome"])
		require.NotNil(t, find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "node:"+testMediaNode+":2026-10-12T10:00:00Z").Rooms)
	})

	t.Run("only standard participants count as participants", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		created := at("0s")
		a := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		b := participantInfo("PA_b", "bob", livekit.ParticipantInfo_STANDARD, at("1s"))
		recorder := participantInfo("PA_e", "EG_1", livekit.ParticipantInfo_EGRESS, at("1s"))
		camera := participantInfo("PA_i", "cam-1", livekit.ParticipantInfo_INGRESS, at("1s"))
		agent := participantInfo("PA_g", "agent-1", livekit.ParticipantInfo_AGENT, at("1s"))
		phone := participantInfo("PA_s", "sip_+34600000000", livekit.ParticipantInfo_SIP, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{
			ev.roomCreated("RM_1", "r", created),
			ev.joined("RM_1", a, at("1s")), ev.active("RM_1", a, at("2s")),
			ev.joined("RM_1", b, at("1s")), ev.active("RM_1", b, at("2s")),
			ev.joined("RM_1", recorder, at("1s")), ev.active("RM_1", recorder, at("2s")),
			ev.joined("RM_1", camera, at("1s")), ev.active("RM_1", camera, at("2s")),
			ev.joined("RM_1", agent, at("1s")), ev.active("RM_1", agent, at("2s")),
			ev.joined("RM_1", phone, at("1s")),
			ev.aborted("RM_1", phone, at("20s"), livekit.DisconnectReason_CONNECTION_TIMEOUT),
			ev.left("RM_1", agent, at("10m"), livekit.DisconnectReason_CLIENT_INITIATED),
		})
		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.Equal(t, map[string]int32{"STANDARD": 2, "EGRESS": 1, "INGRESS": 1, "AGENT": 1, "SIP": 1}, room.Participants.ByKind)
		require.EqualValues(t, 2, room.Participants.Sessions)
		require.EqualValues(t, 2, room.Participants.Unique)
		require.EqualValues(t, 2, room.Participants.Active)
		require.EqualValues(t, 2, room.Participants.PeakActive)
		require.EqualValues(t, 0, room.Participants.Aborted)

		hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z")
		require.Equal(t, map[string]int32{"STANDARD": 2, "EGRESS": 1, "INGRESS": 1, "AGENT": 1, "SIP": 1}, hourly.Participants.ByKind)
		require.EqualValues(t, 2, *hourly.Participants.Joined)
		require.EqualValues(t, 2, *hourly.Participants.Active)
		require.EqualValues(t, 2, hourly.Participants.BySDK["JS"])
		require.Nil(t, hourly.Participants.Aborted)
		require.Nil(t, hourly.Participants.Left)
		require.Nil(t, hourly.Participants.Minutes)
		require.Empty(t, hourly.Participants.DisconnectReasons)
		fiveMinute := find[schema.FiveMinuteRollupDocument](env, schema.CollectionAnalytics5Min, "global:2026-10-12T10:00:00Z")
		require.EqualValues(t, 2, *fiveMinute.Participants.Joined)
		require.EqualValues(t, 2, *fiveMinute.Participants.Active)
		require.Nil(t, fiveMinute.Participants.Aborted)

		env.apply([]*livekit.AnalyticsEvent{
			ev.left("RM_1", camera, at("15m"), livekit.DisconnectReason_CLIENT_INITIATED),
			ev.roomEnded("RM_1", "r", created, at("20m"), livekit.RoomEndReason_ROOM_END_API_DELETE),
		})
		hourly = find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z")
		require.EqualValues(t, 2, *hourly.Rooms.ParticipantsSum)
		require.Nil(t, hourly.Participants.Left)
	})

	t.Run("events out of order and status never regresses", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		a := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("5s"))
		env.apply([]*livekit.AnalyticsEvent{ev.active("RM_1", a, at("6s"))})
		env.apply([]*livekit.AnalyticsEvent{ev.joined("RM_1", a, at("5s"))})
		participant := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a")
		require.Equal(t, "active", participant.Status)
		require.True(t, at("5s").Equal(participant.JoinedAt))
		require.True(t, at("6s").Equal(*participant.ActiveAt))
		require.Equal(t, "JS", participant.Client.SDK)
		require.Equal(t, "Chrome", participant.Client.Browser)
		require.EqualValues(t, 1, count(env, schema.CollectionParticipants, bson.D{}))
	})

	t.Run("replayed batch changes nothing", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		a := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("5s"))
		camera := cameraTrack("TR_cam")
		egress := roomCompositeEgress("EG_1", "RM_1", livekit.EgressStatus_EGRESS_STARTING, at("10s"), at("10s"))
		batch := []*livekit.AnalyticsEvent{
			ev.roomCreated("RM_1", "r", at("0s")),
			ev.joined("RM_1", a, at("5s")), ev.active("RM_1", a, at("6s")),
			ev.track(livekit.AnalyticsEventType_TRACK_PUBLISHED, "RM_1", a, camera, at("7s")),
			ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED, egress, at("10s")),
			ev.left("RM_1", a, at("1m"), livekit.DisconnectReason_CLIENT_INITIATED),
		}
		env.apply(batch)
		first := snapshot(env, allCollections...)

		env.p.cache = newRefCache()
		env.apply(batch)
		require.Equal(t, first, snapshot(env, allCollections...))

		reversed := make([]*livekit.AnalyticsEvent, len(batch))
		for i, event := range batch {
			reversed[len(batch)-1-i] = event
		}
		other := newEnv(t, client)
		other.apply(reversed)
		room := find[schema.RoomDocument](other, schema.CollectionRooms, "RM_1")
		require.EqualValues(t, 1, room.Participants.Sessions)
		require.EqualValues(t, 1, room.Tracks.Published)
		require.EqualValues(t, 1, room.Egress.Total)
		require.True(t, room.Features.Egress)
		participant := find[schema.ParticipantDocument](other, schema.CollectionParticipants, "PA_a")
		require.Equal(t, "left", participant.Status)
	})

	t.Run("aborted connection, reconnect and resume", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		carol := participantInfo("PA_c", "carol", livekit.ParticipantInfo_STANDARD, at("1s"))
		bob1 := participantInfo("PA_b1", "bob", livekit.ParticipantInfo_STANDARD, at("1s"))
		bob2 := participantInfo("PA_b2", "bob", livekit.ParticipantInfo_STANDARD, at("30s"))
		resumed := ev.participant(livekit.AnalyticsEventType_PARTICIPANT_RESUMED, "RM_1", bob2, at("40s"))
		env.apply([]*livekit.AnalyticsEvent{
			ev.joined("RM_1", carol, at("1s")),
			ev.aborted("RM_1", carol, at("20s"), livekit.DisconnectReason_CONNECTION_TIMEOUT),
			ev.joined("RM_1", bob1, at("1s")), ev.active("RM_1", bob1, at("2s")),
			ev.left("RM_1", bob1, at("29s"), livekit.DisconnectReason_DUPLICATE_IDENTITY),
			ev.joined("RM_1", bob2, at("30s")), ev.active("RM_1", bob2, at("31s")),
			resumed,
		})
		aborted := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_c")
		require.Equal(t, "aborted", aborted.Status)
		require.Nil(t, aborted.ActiveAt)
		require.True(t, at("20s").Equal(*aborted.LeftAt))
		require.Equal(t, "CONNECTION_TIMEOUT", aborted.DisconnectReason)

		first := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_b1")
		second := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_b2")
		require.Equal(t, "left", first.Status)
		require.Equal(t, "active", second.Status)
		require.EqualValues(t, 1, second.Resumes)
		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.EqualValues(t, 3, room.Participants.Sessions)
		require.EqualValues(t, 2, room.Participants.Unique)
		require.EqualValues(t, 1, room.Participants.Aborted)
	})

	t.Run("tracks, subscriptions and the cascade on leave", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		bob := participantInfo("PA_b", "bob", livekit.ParticipantInfo_STANDARD, at("1s"))
		camera, mic := cameraTrack("TR_cam"), microphoneTrack("TR_mic")
		env.apply([]*livekit.AnalyticsEvent{
			ev.roomCreated("RM_1", "r", at("0s")),
			ev.joined("RM_1", alice, at("1s")), ev.active("RM_1", alice, at("2s")),
			ev.joined("RM_1", bob, at("1s")), ev.active("RM_1", bob, at("2s")),
			ev.track(livekit.AnalyticsEventType_TRACK_PUBLISHED, "RM_1", alice, camera, at("3s")),
			ev.track(livekit.AnalyticsEventType_TRACK_PUBLISHED, "RM_1", alice, mic, at("3s")),
			ev.subscription(livekit.AnalyticsEventType_TRACK_SUBSCRIBE_REQUESTED, "RM_1", "PA_b", camera, alice, at("4s")),
			ev.subscription(livekit.AnalyticsEventType_TRACK_SUBSCRIBED, "RM_1", "PA_b", camera, alice, at("5s")),
			ev.subscription(livekit.AnalyticsEventType_TRACK_SUBSCRIBE_REQUESTED, "RM_1", "PA_b", mic, alice, at("4s")),
			ev.subscription(livekit.AnalyticsEventType_TRACK_SUBSCRIBE_FAILED, "RM_1", "PA_b", mic, alice, at("6s")),
		})
		subscription := find[schema.TrackSubscriptionDocument](env, schema.CollectionTrackSubscriptions, "TR_cam:PA_b")
		require.Equal(t, "subscribed", subscription.Status)
		require.Equal(t, "bob", subscription.SubscriberIdentity)
		require.Equal(t, "alice", subscription.PublisherIdentity)
		require.Equal(t, "CAMERA", subscription.Source)
		failed := find[schema.TrackSubscriptionDocument](env, schema.CollectionTrackSubscriptions, "TR_mic:PA_b")
		require.Equal(t, "failed", failed.Status)
		require.NotEmpty(t, failed.Error)
		bobDoc := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_b")
		require.EqualValues(t, 1, bobDoc.Tracks.SubscribeFailures)
		require.EqualValues(t, 1, bobDoc.Tracks.Subscribed)
		aliceDoc := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a")
		require.EqualValues(t, 2, aliceDoc.Tracks.Published)
		require.True(t, aliceDoc.IsPublisher)
		track := find[schema.TrackDocument](env, schema.CollectionTracks, "TR_cam")
		require.EqualValues(t, 1, track.Subscribers.Current)
		require.Equal(t, "alice", track.Identity)

		env.apply([]*livekit.AnalyticsEvent{ev.left("RM_1", alice, at("1m"), livekit.DisconnectReason_CLIENT_INITIATED)})
		track = find[schema.TrackDocument](env, schema.CollectionTracks, "TR_cam")
		require.Equal(t, "unpublished", track.Status)
		require.True(t, at("1m").Equal(*track.UnpublishedAt))
		require.Equal(t, int64(57_000), *track.DurationMs)
		subscription = find[schema.TrackSubscriptionDocument](env, schema.CollectionTrackSubscriptions, "TR_cam:PA_b")
		require.Equal(t, "unsubscribed", subscription.Status)
		require.True(t, at("1m").Equal(*subscription.UnsubscribedAt))
	})

	t.Run("egress completes without credentials", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		started := at("10s")
		info := func(status livekit.EgressStatus, updated time.Time) *livekit.EgressInfo {
			return roomCompositeEgress("EG_1", "RM_1", status, started, updated)
		}
		ended := info(livekit.EgressStatus_EGRESS_COMPLETE, at("5m"))
		ended.EndedAt = at("5m").UnixNano()
		ended.FileResults = []*livekit.FileInfo{{Filename: "room.mp4", Location: "https://storage.example.com/room.mp4",
			Size: 3_000_000_000, Duration: (290 * time.Second).Nanoseconds(), StartedAt: at("11s").UnixNano(), EndedAt: at("5m").UnixNano()}}
		ended.StreamResults = []*livekit.StreamInfo{{Url: "rtmps://user:pw@live.example.com/app/live_SECRET?token=abc", Status: livekit.StreamInfo_FINISHED}}
		env.apply([]*livekit.AnalyticsEvent{
			ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED, info(livekit.EgressStatus_EGRESS_STARTING, started), started),
			ev.egress(livekit.AnalyticsEventType_EGRESS_UPDATED, info(livekit.EgressStatus_EGRESS_ACTIVE, at("12s")), at("12s")),
			ev.egress(livekit.AnalyticsEventType_EGRESS_UPDATED, info(livekit.EgressStatus_EGRESS_ENDING, at("4m50s")), at("4m50s")),
			ev.egress(livekit.AnalyticsEventType_EGRESS_ENDED, ended, at("5m")),
		})
		egress := find[schema.EgressDocument](env, schema.CollectionEgresses, "EG_1")
		require.Equal(t, "complete", egress.Status)
		require.Equal(t, "room_composite", egress.Type)
		require.Len(t, egress.StatusHistory, 4)
		require.Equal(t, "room.mp4", egress.Outputs[0].Filename)
		require.Equal(t, int64(3_000_000_000), *egress.Outputs[0].Size)
		require.Equal(t, int64(290_000), *egress.Outputs[0].DurationMs)
		require.Equal(t, "rtmps://live.example.com/app/***", egress.Outputs[1].URL)
		require.Equal(t, int64(290_000), *egress.DurationMs)
		require.Equal(t, "H264_720P_30", egress.Request.Preset)
		require.Equal(t, bson.D{{Key: "room_composite", Value: bson.D{
			{Key: "room_name", Value: "room-RM_1"},
			{Key: "layout", Value: "speaker"},
			{Key: "custom_base_url", Value: "https://layouts.example.com/app/***"},
			{Key: "options", Value: bson.D{{Key: "preset", Value: "H264_720P_30"}}},
			{Key: "file_outputs", Value: bson.A{bson.D{{Key: "filepath", Value: "recordings/room.mp4"}}}},
			{Key: "stream_outputs", Value: bson.A{bson.D{{Key: "urls", Value: bson.A{"rtmps://live.example.com/app/***"}}}}},
		}}}, egress.RawRequest)
		require.Equal(t, "room-RM_1", egress.RoomName)
		require.Equal(t, "room-rm_1", egress.RoomNameLower)

		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.True(t, room.Features.Egress)
		require.EqualValues(t, 1, room.Egress.Total)
		require.EqualValues(t, 0, room.Egress.Active)

		assertNoSecrets(t, env)
	})

	t.Run("an egress status reported twice keeps its first time", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		started := at("10s")
		first, second := at("12s"), at("12s").Add(13*time.Millisecond)
		for id, order := range map[string][]time.Time{"EG_1": {first, second}, "EG_2": {second, first}} {
			batch := []*livekit.AnalyticsEvent{ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED,
				roomCompositeEgress(id, "RM_1", livekit.EgressStatus_EGRESS_STARTING, started, started), started)}
			for _, updated := range order {
				batch = append(batch, ev.egress(livekit.AnalyticsEventType_EGRESS_UPDATED,
					roomCompositeEgress(id, "RM_1", livekit.EgressStatus_EGRESS_ACTIVE, started, updated), updated))
			}
			env.apply(batch)
			egress := find[schema.EgressDocument](env, schema.CollectionEgresses, id)
			require.Len(t, egress.StatusHistory, 2, id)
			require.Equal(t, "active", egress.StatusHistory[1].Status, id)
			require.True(t, first.Equal(egress.StatusHistory[1].At), id)
		}
	})

	t.Run("a session without a browser counts as none", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		joined := ev.joined("RM_1", participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("5s")), at("5s"))
		joined.ClientInfo = &livekit.ClientInfo{Sdk: livekit.ClientInfo_GO, Version: "2.19.0", Os: "linux"}
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "Daily", at("0s")), joined})
		hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z")
		require.Equal(t, map[string]int32{"none": 1}, hourly.Participants.ByBrowser)
	})

	t.Run("agents keep the attributes that name them without store_metadata", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		openvidu := participantInfo("PA_o", "agent-AJ_o", livekit.ParticipantInfo_AGENT, at("1s"))
		openvidu.Attributes = map[string]string{
			"lk.agent_name": "speech-processing", "openvidu.agent": "speech-processing", "openvidu.agent.version": "3.10.0",
			"lk.agent.name": "SECRET-sdk-name", "lk.agent.state": "SECRET-listening", "openvidu.agents": "SECRET-prefix",
			"customer": "SECRET-customer",
		}
		other := participantInfo("PA_d", "agent-AJ_d", livekit.ParticipantInfo_AGENT, at("1s"))
		other.Attributes = map[string]string{"lk.agent_name": "dummy-agent", "lk.agent.state": "SECRET-listening"}
		unnamed := participantInfo("PA_u", "agent-AJ_u", livekit.ParticipantInfo_AGENT, at("1s"))
		unnamed.Attributes = map[string]string{"lk.agent.state": "SECRET-listening"}
		user := participantInfo("PA_s", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		user.Attributes = map[string]string{"lk.agent_name": "SECRET-user", "openvidu.agent": "SECRET-user"}
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "Daily", at("0s")),
			ev.joined("RM_1", openvidu, at("1s")), ev.joined("RM_1", other, at("1s")), ev.joined("RM_1", unnamed, at("1s")),
			ev.joined("RM_1", user, at("1s")), ev.left("RM_1", openvidu, at("9s"), livekit.DisconnectReason_CLIENT_INITIATED)})

		marked := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_o")
		require.Equal(t, map[string]string{
			"lk.agent_name": "speech-processing", "openvidu.agent": "speech-processing", "openvidu.agent.version": "3.10.0",
		}, marked.Attributes)
		require.Empty(t, marked.Metadata)
		require.Equal(t, map[string]string{"lk.agent_name": "dummy-agent"},
			find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_d").Attributes)
		require.Nil(t, find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_u").Attributes)
		require.Nil(t, find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_s").Attributes)
		// raw events keep no attribute without store_metadata, agents' included
		assertNoSecrets(t, env)
		require.Zero(t, count(env, schema.CollectionEvents, bson.D{{Key: "payload.participant.attributes", Value: bson.D{{Key: "$exists", Value: true}}}}))
	})

	t.Run("agents keep every attribute with store_metadata", func(t *testing.T) {
		env := newEnv(t, client, func(c *openviduconfig.AnalyticsConfig) { c.StoreMetadata = true })
		var ev events
		agent := participantInfo("PA_o", "agent-AJ_o", livekit.ParticipantInfo_AGENT, at("1s"))
		agent.Attributes = map[string]string{"lk.agent_name": "speech-processing", "lk.agent.state": "listening", "customer": "acme"}
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "Daily", at("0s")), ev.joined("RM_1", agent, at("1s"))})

		require.Equal(t, agent.Attributes, find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_o").Attributes)
	})

	t.Run("values starting with $ stored as they are", func(t *testing.T) {
		env := newEnv(t, client, func(c *openviduconfig.AnalyticsConfig) { c.StoreMetadata = true })
		var ev events
		created := ev.roomCreated("RM_1", "$room", at("0s"))
		created.Room.Metadata = "$$ROOT"
		alice := participantInfo("PA_a", "$$ROOT", livekit.ParticipantInfo_STANDARD, at("1s"))
		alice.Name = "$name"
		alice.Metadata = "$metadata"
		alice.Attributes = map[string]string{"$role": "$host", "team.name": "$blue"}
		failed := roomCompositeEgress("EG_1", "RM_1", livekit.EgressStatus_EGRESS_FAILED, at("2s"), at("3s"))
		failed.Error = "$error"
		env.apply([]*livekit.AnalyticsEvent{created, ev.joined("RM_1", alice, at("1s")),
			ev.egress(livekit.AnalyticsEventType_EGRESS_ENDED, failed, at("3s"))})

		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.Equal(t, "$room", room.Name)
		require.Equal(t, "$$ROOT", room.Config.Metadata)
		require.EqualValues(t, 1, room.Participants.Unique)
		participant := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a")
		require.Equal(t, "$$ROOT", participant.Identity)
		require.Equal(t, "$name", participant.Name)
		require.Equal(t, "$metadata", participant.Metadata)
		require.Equal(t, map[string]string{"$role": "$host", "team.name": "$blue"}, participant.Attributes)
		require.Equal(t, "$error", find[schema.EgressDocument](env, schema.CollectionEgresses, "EG_1").Error)
	})

	t.Run("raw request stored as encoded", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		track := &livekit.EgressInfo{EgressId: "EG_t", RoomId: "RM_1", RoomName: "room-RM_1",
			Status: livekit.EgressStatus_EGRESS_STARTING, StartedAt: at("0s").UnixNano(),
			Request: &livekit.EgressInfo_Track{Track: &livekit.TrackEgressRequest{RoomName: "room-RM_1", TrackId: "TR_1",
				Output: &livekit.TrackEgressRequest_File{File: &livekit.DirectFileOutput{Filepath: "$recordings/{track_id}.ogg",
					Output: &livekit.DirectFileOutput_S3{S3: &livekit.S3Upload{AccessKey: "SECRET-access", Secret: "SECRET-secret"}}}}}}}
		env.apply([]*livekit.AnalyticsEvent{ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED, track, at("0s"))})
		egress := find[schema.EgressDocument](env, schema.CollectionEgresses, "EG_t")
		require.Equal(t, bson.D{{Key: "track", Value: bson.D{
			{Key: "room_name", Value: "room-RM_1"},
			{Key: "track_id", Value: "TR_1"},
			{Key: "output", Value: bson.D{{Key: "file", Value: bson.D{{Key: "filepath", Value: "$recordings/{track_id}.ogg"}}}}},
		}}}, egress.RawRequest)
		assertNoSecrets(t, env)
	})

	t.Run("web egress without room", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		web := &livekit.EgressInfo{EgressId: "EG_w", Status: livekit.EgressStatus_EGRESS_STARTING, StartedAt: at("0s").UnixNano(),
			Request: &livekit.EgressInfo_Web{Web: &livekit.WebEgressRequest{Url: "https://app.example.com/view?token=SECRET-jwt"}}}
		env.apply([]*livekit.AnalyticsEvent{ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED, web, at("0s"))})
		egress := find[schema.EgressDocument](env, schema.CollectionEgresses, "EG_w")
		require.Empty(t, egress.RoomSID)
		require.Empty(t, egress.RoomNameLower)
		require.Equal(t, "web", egress.Type)
		require.Equal(t, "https://app.example.com/***", egress.Request.URL)
		require.Zero(t, count(env, schema.CollectionRooms, bson.D{}))
		assertNoSecrets(t, env)
	})

	t.Run("ingress session lost on the server ends in error", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		publishing := rtmpIngress("IN_1", "room-RM_1", ingressState(livekit.IngressState_ENDPOINT_PUBLISHING, "RM_1", "res_1", at("1m"), time.Time{}))
		lost := rtmpIngress("IN_1", "room-RM_1", ingressState(livekit.IngressState_ENDPOINT_INACTIVE, "RM_1", "res_1", at("1m"), at("3m")))
		lost.State.Error = "ingress lost"
		env.apply([]*livekit.AnalyticsEvent{
			ev.ingress(livekit.AnalyticsEventType_INGRESS_STARTED, publishing, at("1m")),
			ev.ingress(livekit.AnalyticsEventType_INGRESS_ENDED, lost, at("3m")),
		})
		session := find[schema.IngressSessionDocument](env, schema.CollectionIngressSessions, "res_1")
		require.Equal(t, "error", session.Status)
		require.Equal(t, "ingress lost", session.Error)
		require.Equal(t, "event", session.ClosedBy)
		require.Equal(t, "inactive", find[schema.IngressDocument](env, schema.CollectionIngresses, "IN_1").Status)
	})

	t.Run("reusable ingress with two sessions and no stream key", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		created := rtmpIngress("IN_1", "room-RM_1", &livekit.IngressState{})
		first := rtmpIngress("IN_1", "room-RM_1", ingressState(livekit.IngressState_ENDPOINT_PUBLISHING, "RM_1", "res_1", at("1m"), time.Time{}))
		firstEnd := rtmpIngress("IN_1", "room-RM_1", ingressState(livekit.IngressState_ENDPOINT_INACTIVE, "RM_1", "res_1", at("1m"), at("3m")))
		second := rtmpIngress("IN_1", "room-RM_1", ingressState(livekit.IngressState_ENDPOINT_PUBLISHING, "RM_1", "res_2", at("5m"), time.Time{}))
		secondEnd := rtmpIngress("IN_1", "room-RM_1", ingressState(livekit.IngressState_ENDPOINT_COMPLETE, "RM_1", "res_2", at("5m"), at("6m")))
		env.apply([]*livekit.AnalyticsEvent{
			ev.ingress(livekit.AnalyticsEventType_INGRESS_CREATED, created, at("0s")),
			ev.ingress(livekit.AnalyticsEventType_INGRESS_STARTED, first, at("1m")),
			ev.ingress(livekit.AnalyticsEventType_INGRESS_ENDED, firstEnd, at("3m")),
			ev.ingress(livekit.AnalyticsEventType_INGRESS_STARTED, second, at("5m")),
			ev.ingress(livekit.AnalyticsEventType_INGRESS_ENDED, secondEnd, at("6m")),
		})
		ingress := find[schema.IngressDocument](env, schema.CollectionIngresses, "IN_1")
		require.EqualValues(t, 2, ingress.Sessions)
		require.Equal(t, int64(180_000), ingress.TotalDurationMs)
		require.Equal(t, "rtmp", ingress.InputType)
		require.Equal(t, "complete", ingress.Status)
		require.EqualValues(t, 2, count(env, schema.CollectionIngressSessions, bson.D{}))
		session := find[schema.IngressSessionDocument](env, schema.CollectionIngressSessions, "res_1")
		require.Equal(t, "inactive", session.Status)
		require.Equal(t, "RM_1", session.RoomSID)
		require.Equal(t, int64(120_000), *session.DurationMs)
		require.EqualValues(t, 1, count(env, schema.CollectionEvents, bson.D{{Key: "ingressSessionId", Value: "res_2"}, {Key: "type", Value: "INGRESS_STARTED"}}))
		hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z")
		require.EqualValues(t, 2, *hourly.Ingress.SessionsStarted)
		require.Equal(t, map[string]int32{"rtmp": 2}, hourly.Ingress.ByInputType)
		assertNoSecrets(t, env)
	})

	t.Run("raw events: default stored types and ingress update gate", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		camera := cameraTrack("TR_cam")
		apiCall := ev.next(livekit.AnalyticsEventType_API_CALL, at("1s"))
		apiCall.ApiCall = &livekit.APICallInfo{Service: "RoomService", Method: "CreateRoom", TwirpErrorCode: "already_exists"}
		publishing := ingressState(livekit.IngressState_ENDPOINT_PUBLISHING, "RM_1", "res_1", at("1m"), time.Time{})
		buffering := ingressState(livekit.IngressState_ENDPOINT_BUFFERING, "RM_1", "res_1", at("1m"), time.Time{})
		env.apply([]*livekit.AnalyticsEvent{
			ev.roomCreated("RM_1", "r", at("0s")),
			apiCall,
			ev.track(livekit.AnalyticsEventType_TRACK_PUBLISHED_UPDATE, "RM_1", alice, camera, at("2s")),
			ev.track(livekit.AnalyticsEventType_TRACK_MAX_SUBSCRIBED_VIDEO_QUALITY, "RM_1", alice, camera, at("2s")),
			ev.subscription(livekit.AnalyticsEventType_TRACK_SUBSCRIBE_REQUESTED, "RM_1", "PA_b", camera, alice, at("3s")),
			ev.ingress(livekit.AnalyticsEventType_INGRESS_UPDATED, rtmpIngress("IN_1", "room-RM_1", publishing), at("1m")),
			ev.ingress(livekit.AnalyticsEventType_INGRESS_UPDATED, rtmpIngress("IN_1", "room-RM_1", publishing), at("1m10s")),
			ev.ingress(livekit.AnalyticsEventType_INGRESS_UPDATED, rtmpIngress("IN_1", "room-RM_1", buffering), at("1m20s")),
		})
		for _, eventType := range []string{"API_CALL", "TRACK_PUBLISHED_UPDATE", "TRACK_MAX_SUBSCRIBED_VIDEO_QUALITY", "TRACK_SUBSCRIBE_REQUESTED"} {
			require.Zero(t, count(env, schema.CollectionEvents, bson.D{{Key: "type", Value: eventType}}), eventType)
		}
		require.EqualValues(t, 2, count(env, schema.CollectionEvents, bson.D{{Key: "type", Value: "INGRESS_UPDATED"}}))
		require.EqualValues(t, 1, count(env, schema.CollectionEvents, bson.D{{Key: "roomSid", Value: "RM_1"}, {Key: "type", Value: "ROOM_CREATED"}}))

		hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z")
		require.EqualValues(t, 1, hourly.APICalls.ByMethod["RoomService_CreateRoom"])
		require.EqualValues(t, 1, *hourly.APIErrors.Total)
		session := find[schema.IngressSessionDocument](env, schema.CollectionIngressSessions, "res_1")
		require.Equal(t, "buffering", session.Status)
	})

	t.Run("configured stored set", func(t *testing.T) {
		env := newEnv(t, client, func(c *openviduconfig.AnalyticsConfig) {
			c.Events.StoreTypes = "ROOM_CREATED,ROOM_ENDED,API_CALL"
		})
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		apiCall := ev.next(livekit.AnalyticsEventType_API_CALL, at("1s"))
		apiCall.ApiCall = &livekit.APICallInfo{Service: "RoomService", Method: "ListRooms"}
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s")), apiCall})
		require.EqualValues(t, 1, count(env, schema.CollectionEvents, bson.D{}))
		require.EqualValues(t, 1, count(env, schema.CollectionParticipants, bson.D{}))
	})

	t.Run("room resolution", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		created := ev.roomCreated("RM_x", "x", at("0s"))
		created.Room.CreationTimeMs = at("-1s").UnixMilli()
		egress := roomCompositeEgress("EG_1", "RM_y", livekit.EgressStatus_EGRESS_STARTING, at("1s"), at("1s"))
		env.apply([]*livekit.AnalyticsEvent{created, ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED, egress, at("1s"))})
		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_x")
		require.True(t, at("-1s").Equal(room.CreatedAt))
		raw := find[schema.EventDocument](env, schema.CollectionEvents, created.GetId())
		require.Equal(t, "RM_x", raw.RoomSID)
		roomY := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_y")
		require.True(t, roomY.Features.Egress)
		require.EqualValues(t, 1, roomY.Egress.Total)
		require.Equal(t, "room-RM_y", roomY.Name)
	})

	t.Run("stats in participant detail", func(t *testing.T) {
		env := newEnv(t, client)
		env.p.opts.MediaNode = testMediaNode
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s"))})
		flush := at("4m59s")
		env.apply(nil, []*livekit.AnalyticsStat{
			stat("RM_1", "PA_a", "TR_cam", livekit.StreamType_UPSTREAM, flush, 4.3, 1000),
			stat("RM_1", "PA_a", "TR_mic", livekit.StreamType_UPSTREAM, flush, 1.5, 500),
			stat("RM_1", "PA_a", "TR_SG1", livekit.StreamType_UPSTREAM, flush, 0, 70),
		})
		require.EqualValues(t, 1, count(env, schema.CollectionTrackStats, bson.D{}))
		var document schema.TrackStatsDocument
		require.NoError(t, env.db.Collection(schema.CollectionTrackStats).FindOne(context.Background(), bson.D{}).Decode(&document))
		require.Equal(t, int64(1500), document.Bytes)
		require.InDelta(t, 2.9, document.Score, 0.001)
		require.Equal(t, 2.5, document.JitterMs)
		require.Empty(t, document.Meta.TrackSID)
		require.Empty(t, document.MimeType)

		participant := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a")
		require.Equal(t, int64(1570), participant.Bytes.Up, "signal bytes count for the participant")
		require.True(t, flush.Equal(*participant.LastStatsAt))
		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.Equal(t, int64(1500), room.Bytes.Up, "rooms count media bytes only")
		fiveMinutes := find[schema.FiveMinuteRollupDocument](env, schema.CollectionAnalytics5Min, "global:2026-10-12T10:00:00Z")
		require.Equal(t, int64(1500), *fiveMinutes.Bytes.Up)
		require.EqualValues(t, 1, *fiveMinutes.Participants.Connected)
		require.NotNil(t, find[schema.FiveMinuteRollupDocument](env, schema.CollectionAnalytics5Min, "node:"+testMediaNode+":2026-10-12T10:00:00Z").Bytes)
		hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z")
		require.Equal(t, int64(1500), *hourly.Bytes.Up)
	})

	t.Run("a signal-only flush advances the room's lastStatsAt without bytes or quality", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s"))})
		flush := at("30s")
		env.apply(nil, []*livekit.AnalyticsStat{
			stat("RM_1", "PA_a", "TR_SG1", livekit.StreamType_UPSTREAM, flush, 0, 70),
			stat("RM_1", "PA_a", "TR_SG1", livekit.StreamType_DOWNSTREAM, flush, 0, 90),
		})

		require.Zero(t, count(env, schema.CollectionTrackStats, bson.D{}))
		participant := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a")
		require.Equal(t, int64(70), participant.Bytes.Up)
		require.Equal(t, int64(90), participant.Bytes.Down)
		require.True(t, flush.Equal(*participant.LastStatsAt))
		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.True(t, flush.Equal(*room.LastStatsAt))
		require.Zero(t, room.Bytes.Up)
		require.Zero(t, room.Bytes.Down)
		require.Zero(t, room.Quality.ScoreCount)
	})

	t.Run("a node without media statistics marks the rooms and sessions it creates", func(t *testing.T) {
		env := newEnv(t, client)
		env.p.opts.WithoutMediaStats = true
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s"))})

		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.NotNil(t, room.MediaStats)
		require.False(t, *room.MediaStats)
		participant := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a")
		require.NotNil(t, participant.MediaStats)
		require.False(t, *participant.MediaStats)
	})

	t.Run("a node reporting media statistics leaves the mark out", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s"))})

		require.Nil(t, find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1").MediaStats)
		require.Nil(t, find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a").MediaStats)
	})

	t.Run("ROOM_CREATED sets the mark of the node that hosts the room", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		egress := roomCompositeEgress("EG_1", "RM_1", livekit.EgressStatus_EGRESS_STARTING, at("1s"), at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED, egress, at("1s"))})
		require.Nil(t, find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1").MediaStats)

		env.p.opts.WithoutMediaStats = true
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s"))})
		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.NotNil(t, room.MediaStats)
		require.False(t, *room.MediaStats)

		env.p.opts.WithoutMediaStats = false
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s"))})
		require.Nil(t, find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1").MediaStats)
	})

	t.Run("a labelled node stamps its rooms and sessions and keys the node scope by the label", func(t *testing.T) {
		env := newEnv(t, client)
		env.p.opts.MediaNode = "mn-a"
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s"))})
		env.apply(nil, []*livekit.AnalyticsStat{stat("RM_1", "PA_a", "TR_cam", livekit.StreamType_UPSTREAM, at("30s"), 4.5, 1000)})

		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.Equal(t, testNode, room.NodeID)
		require.Equal(t, "mn-a", room.MediaNode)
		require.Equal(t, "mn-a", find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a").MediaNode)
		hour := at("0s").Truncate(time.Hour).Format(time.RFC3339)
		hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "node:mn-a:"+hour)
		require.EqualValues(t, 1, *hourly.Rooms.Created)
		require.EqualValues(t, 1000, *hourly.Bytes.Up)
		require.Zero(t, count(env, schema.CollectionAnalyticsHourly, bson.D{{Key: "scope", Value: "node:" + testNode}}))
	})

	t.Run("a server without a label writes no mediaNode and only the global scope", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s"))})
		env.apply(nil, []*livekit.AnalyticsStat{stat("RM_1", "PA_a", "TR_cam", livekit.StreamType_UPSTREAM, at("30s"), 4.5, 1000)})

		require.Empty(t, find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1").MediaNode)
		require.Empty(t, find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a").MediaNode)
		for _, collection := range []string{schema.CollectionAnalytics5Min, schema.CollectionAnalyticsHourly, schema.CollectionAnalyticsDaily} {
			require.Zero(t, count(env, collection, bson.D{{Key: "scope", Value: bson.D{{Key: "$ne", Value: "global"}}}}), collection)
		}
	})

	t.Run("ROOM_CREATED sets the label of the node that hosts the room", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		env.p.opts.MediaNode = "mn-b"
		egress := roomCompositeEgress("EG_1", "RM_1", livekit.EgressStatus_EGRESS_STARTING, at("1s"), at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED, egress, at("1s"))})
		require.Equal(t, "mn-b", find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1").MediaNode)

		env.p.opts.MediaNode = "mn-a"
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s"))})
		require.Equal(t, "mn-a", find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1").MediaNode)
	})

	t.Run("the batch counts the rooms it hosts once, from ROOM_CREATED only", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		egress := roomCompositeEgress("EG_1", "RM_3", livekit.EgressStatus_EGRESS_STARTING, at("1s"), at("1s"))
		created := []*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r1", at("0s")), ev.roomCreated("RM_2", "r2", at("0s"))}
		env.apply([]*livekit.AnalyticsEvent{ev.egress(livekit.AnalyticsEventType_EGRESS_STARTED, egress, at("1s"))})
		require.Zero(t, env.p.takeHostedRooms())

		env.apply(created)
		env.apply(created)
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_3", "r3", at("2s"))})
		require.Equal(t, int32(3), env.p.takeHostedRooms())
		require.Zero(t, env.p.takeHostedRooms())
	})

	t.Run("a server without media statistics marks its rollup buckets as missing", func(t *testing.T) {
		env := newEnv(t, client)
		env.p.opts.WithoutMediaStats = true
		var ev events
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s"))})

		for _, document := range []struct{ collection, id string }{
			{schema.CollectionAnalytics5Min, "global:2026-10-12T10:00:00Z"},
			{schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z"},
			{schema.CollectionAnalyticsDaily, "global:2026-10-12T00:00:00Z"},
		} {
			marks := find[schema.HourlyRollupDocument](env, document.collection, document.id).MediaStats
			require.NotNil(t, marks, document.collection)
			require.True(t, marks.Missing, document.collection)
			require.False(t, marks.Reported, document.collection)
		}
	})

	t.Run("servers of both kinds mark the global bucket with both and their node scopes with their own", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		env.p.opts.MediaNode = "mn-a"
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r1", at("0s"))})
		env.p.opts.MediaNode = "mn-b"
		env.p.opts.WithoutMediaStats = true
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_2", "r2", at("1s"))})

		marks := func(scope string) schema.RollupMediaStats {
			document := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, scope+":2026-10-12T10:00:00Z")
			require.NotNil(t, document.MediaStats, scope)
			return *document.MediaStats
		}
		require.Equal(t, schema.RollupMediaStats{Reported: true, Missing: true}, marks("global"))
		require.Equal(t, schema.RollupMediaStats{Reported: true}, marks("node:mn-a"))
		require.Equal(t, schema.RollupMediaStats{Missing: true}, marks("node:mn-b"))
	})

	t.Run("stats of a server reporting them mark their buckets as reported", func(t *testing.T) {
		env := newEnv(t, client)
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.joined("RM_1", alice, at("1s"))})
		env.apply(nil, []*livekit.AnalyticsStat{stat("RM_1", "PA_a", "TR_cam", livekit.StreamType_UPSTREAM, at("30s"), 4.5, 1000)})

		five := find[schema.FiveMinuteRollupDocument](env, schema.CollectionAnalytics5Min, "global:2026-10-12T10:00:00Z")
		require.Equal(t, &schema.RollupMediaStats{Reported: true}, five.MediaStats)
		daily := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsDaily, "global:2026-10-12T00:00:00Z")
		require.Equal(t, &schema.RollupMediaStats{Reported: true}, daily.MediaStats)
	})

	t.Run("a replayed batch keeps the media statistics marks", func(t *testing.T) {
		env := newEnv(t, client)
		env.p.opts.WithoutMediaStats = true
		var ev events
		batch := []*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s"))}
		env.apply(batch)
		env.apply(batch)

		hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, "global:2026-10-12T10:00:00Z")
		require.Equal(t, &schema.RollupMediaStats{Missing: true}, hourly.MediaStats)
		require.EqualValues(t, 1, *hourly.Rooms.Created)
	})

	t.Run("stats in track detail, quality bands and connected participants", func(t *testing.T) {
		env := newEnv(t, client, func(c *openviduconfig.AnalyticsConfig) { c.StatsDetail = "track" })
		var ev events
		var batch []*livekit.AnalyticsEvent
		for _, sid := range []string{"PA_a", "PA_b", "PA_c"} {
			batch = append(batch, ev.joined("RM_1", participantInfo(sid, sid, livekit.ParticipantInfo_STANDARD, at("1s")), at("1s")))
		}
		env.apply(batch)
		env.apply(nil, []*livekit.AnalyticsStat{
			stat("RM_1", "PA_a", "TR_a1", livekit.StreamType_UPSTREAM, at("1m"), 4.3, 100),
			stat("RM_1", "PA_b", "TR_b1", livekit.StreamType_UPSTREAM, at("1m"), 1.5, 100),
			stat("RM_1", "PA_c", "TR_c1", livekit.StreamType_UPSTREAM, at("1m"), 1.0, 100),
		}, []*livekit.AnalyticsStat{
			stat("RM_1", "PA_a", "TR_a1", livekit.StreamType_UPSTREAM, at("1m30s"), 0, 100),
			stat("RM_1", "PA_b", "TR_b1", livekit.StreamType_UPSTREAM, at("1m30s"), 0, 100),
		})
		require.EqualValues(t, 5, count(env, schema.CollectionTrackStats, bson.D{{Key: "meta.trackSid", Value: bson.D{{Key: "$exists", Value: true}}}}))
		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.InDelta(t, 6.8, room.Quality.ScoreSum, 0.001)
		require.EqualValues(t, 3, room.Quality.ScoreCount)
		require.EqualValues(t, 2, room.Quality.Poor)
		require.EqualValues(t, 1, room.Quality.Lost)
		require.InDelta(t, 1.0, *room.Quality.MinScore, 0.001)
		fiveMinutes := find[schema.FiveMinuteRollupDocument](env, schema.CollectionAnalytics5Min, "global:2026-10-12T10:00:00Z")
		require.EqualValues(t, 3, *fiveMinutes.Participants.Connected)
	})

	t.Run("hourly counters do not double count a replayed batch", func(t *testing.T) {
		env := newEnv(t, client)
		env.p.opts.MediaNode = testMediaNode
		var ev events
		var batch []*livekit.AnalyticsEvent
		for i, sid := range []string{"RM_1", "RM_2", "RM_3", "RM_4", "RM_5"} {
			created := at("0s").Add(time.Duration(i) * time.Minute)
			batch = append(batch, ev.roomCreated(sid, sid, created))
			if i < 2 {
				batch = append(batch, ev.roomEnded(sid, sid, created, created.Add(10*time.Minute), livekit.RoomEndReason_ROOM_END_IDLE_TIMEOUT))
			}
		}
		env.apply(batch)
		env.p.cache = newRefCache()
		env.apply(batch)
		for _, id := range []string{"global:2026-10-12T10:00:00Z", "node:" + testMediaNode + ":2026-10-12T10:00:00Z"} {
			hourly := find[schema.HourlyRollupDocument](env, schema.CollectionAnalyticsHourly, id)
			require.EqualValues(t, 5, *hourly.Rooms.Created, id)
			require.EqualValues(t, 2, *hourly.Rooms.Ended, id)
			require.EqualValues(t, 2, hourly.Rooms.EndedByReason["IDLE_TIMEOUT"], id)
		}
	})

	t.Run("IP addresses kept on request", func(t *testing.T) {
		storeIP := false
		env := newEnv(t, client, func(c *openviduconfig.AnalyticsConfig) { c.AnonymizeIP = &storeIP })
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s"))})
		participant := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a")
		require.NotEmpty(t, participant.Client.Address)
	})

	t.Run("personal data flags", func(t *testing.T) {
		env := newEnv(t, client, func(c *openviduconfig.AnalyticsConfig) { c.StoreMetadata = true })
		var ev events
		alice := participantInfo("PA_a", "alice", livekit.ParticipantInfo_STANDARD, at("1s"))
		env.apply([]*livekit.AnalyticsEvent{ev.roomCreated("RM_1", "r", at("0s")), ev.joined("RM_1", alice, at("1s"))})
		participant := find[schema.ParticipantDocument](env, schema.CollectionParticipants, "PA_a")
		require.Empty(t, participant.Client.Address)
		require.Equal(t, "participant metadata", participant.Metadata)
		require.Equal(t, "host", participant.Attributes["role"])
		room := find[schema.RoomDocument](env, schema.CollectionRooms, "RM_1")
		require.Equal(t, "room metadata", room.Config.Metadata)
		raw, err := env.db.Collection(schema.CollectionEvents).CountDocuments(context.Background(),
			bson.D{{Key: "payload.client_info.address", Value: bson.D{{Key: "$exists", Value: true}}}})
		require.NoError(t, err)
		require.Zero(t, raw)
	})
}

// assertNoSecrets fails when any stored document holds a value marked SECRET by the builders.
func assertNoSecrets(t *testing.T, env *testEnv) {
	t.Helper()
	for collection, documents := range snapshot(env, allCollections...) {
		for _, document := range documents {
			encoded, err := bson.MarshalExtJSON(document, false, false)
			require.NoError(t, err)
			require.False(t, strings.Contains(string(encoded), "SECRET"), "%s holds a secret: %s", collection, encoded)
		}
	}
}
