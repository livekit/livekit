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

package projection

import (
	"slices"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/livekit/livekit-server/openvidu/analytics/schema"
)

const (
	participantJoining = "joining"
	participantActive  = "active"
	participantLeft    = "left"
	participantAborted = "aborted"
)

var participantRanks = []rank{{participantJoining, 0}, {participantActive, 1}, {participantLeft, 2}, {participantAborted, 2}}

var agentKind = livekit.ParticipantInfo_AGENT.String()

// namesAgent reports whether an agent attribute is stored without store_metadata: the agent name the server stamps
// on every agent, and the mark OpenVidu agents set, so the dashboard can tell them apart from other agents.
func namesAgent(key string) bool {
	return key == "lk.agent_name" || key == "openvidu.agent" || strings.HasPrefix(key, "openvidu.agent.")
}

// attributesDocument holds the attributes that keep reports true for, sorted by key; nil when there are none.
func attributesDocument(attributes map[string]string, keep func(key string) bool) bson.D {
	var document bson.D
	for _, key := range sortedKeys(attributes) {
		if keep(key) {
			document = append(document, bson.E{Key: key, Value: attributes[key]})
		}
	}
	return document
}

func (p *projector) participant() {
	info := p.event.GetParticipant()
	sid := firstNonEmpty(p.event.GetParticipantId(), info.GetSid())
	roomSid, roomName := roomRef(p.event)
	if sid == "" || roomSid == "" {
		return
	}
	kind := info.GetKind().String()
	identity := firstNonEmpty(info.GetIdentity(), p.refs.Identities[sid])
	eventType := p.event.GetType()

	f := newFields()
	if identity != "" {
		f.set("identity", keep("identity", identity))
		f.set("identityLower", keep("identityLower", strings.ToLower(identity)))
	}
	if info.GetName() != "" {
		f.set("name", info.GetName())
	}
	f.set("roomSid", keep("roomSid", roomSid))
	if roomName != "" {
		f.set("roomName", keep("roomName", roomName))
	}
	f.set("kind", keep("kind", kind))
	if len(info.GetKindDetails()) > 0 {
		details := bson.A{}
		for _, detail := range info.GetKindDetails() {
			details = append(details, detail.String())
		}
		f.set("kindDetails", details)
	}
	f.set("joinedAt", minDate("joinedAt", joinTime(info, p.ts)))
	f.set("nodeId", keep("nodeId", firstNonEmpty(p.event.GetClientMeta().GetNode(), p.event.GetNodeId())))
	p.markFirstWriter(f)
	if region := firstNonEmpty(info.GetRegion(), p.event.GetClientMeta().GetRegion()); region != "" {
		f.set("region", region)
	}
	if permission := info.GetPermission(); permission != nil {
		f.set("permission", permissionDocument(permission))
	}
	f.set("isPublisher", or(keep("isPublisher", false), info.GetIsPublisher()))
	if p.opts.Encode.StoreMetadata {
		if info.GetMetadata() != "" {
			f.set("metadata", info.GetMetadata())
		}
		if attributes := attributesDocument(info.GetAttributes(), func(string) bool { return true }); attributes != nil {
			f.set("attributes", literal(attributes))
		}
	} else if kind == agentKind {
		if attributes := attributesDocument(info.GetAttributes(), namesAgent); attributes != nil {
			f.set("attributes", literal(attributes))
		}
	}
	f.set("lastEventAt", maxDate("lastEventAt", p.ts))
	f.set("schemaVersion", int32(schema.SchemaVersion))

	room := p.roomTouch(roomName)
	room.set("internal.participantSids", union("internal.participantSids", sid))
	standard := kind == StandardKind
	if identity != "" && standard {
		room.set("internal.identities", union("internal.identities", identity))
	}
	room.set("internal.kinds."+kind, union("internal.kinds."+kind, sid))

	switch eventType {
	case livekit.AnalyticsEventType_PARTICIPANT_JOINED:
		f.set("status", ranked("status", participantJoining, participantRanks))
		if client := p.event.GetClientInfo(); client != nil {
			f.set("client", clientDocument(client, p.opts.Encode.AnonymizeIP))
		}
	case livekit.AnalyticsEventType_PARTICIPANT_ACTIVE, livekit.AnalyticsEventType_PARTICIPANT_RESUMED:
		f.set("status", ranked("status", participantActive, participantRanks))
		f.set("activeAt", minDate("activeAt", p.ts))
		meta := p.event.GetClientMeta()
		if eventType == livekit.AnalyticsEventType_PARTICIPANT_ACTIVE {
			if meta.GetClientConnectTime() > 0 {
				f.set("connectTimeMs", int32(meta.GetClientConnectTime()))
			}
			if connectionType := meta.GetConnectionType(); slices.Contains([]string{"udp", "tcp", "turn"}, connectionType) {
				f.set("connectionType", connectionType)
			}
		} else {
			f.set("internal.resumeEventIds", union("internal.resumeEventIds", p.event.GetId()))
		}
		if standard {
			room.set("internal.activeSids", union("internal.activeSids", sid))
			room.set("participants.peakActive", maxOf("participants.peakActive", activeAfter(sid)))
			room.set("participants.peakPublishers", maxOf("participants.peakPublishers",
				publishersAfter(union("internal.activeSids", sid), emptyArray("internal.senderSids"))))
		}
	case livekit.AnalyticsEventType_PARTICIPANT_LEFT, livekit.AnalyticsEventType_PARTICIPANT_CONNECTION_ABORTED:
		status := participantLeft
		if eventType == livekit.AnalyticsEventType_PARTICIPANT_CONNECTION_ABORTED {
			status = participantAborted
			if standard {
				room.set("internal.abortedSids", union("internal.abortedSids", sid))
			}
		}
		closedByEvent := eq(field("closedBy"), byEvent)
		f.set("status", cond(closedByEvent, field("status"), status))
		f.set("leftAt", cond(closedByEvent, field("leftAt"), p.ts))
		f.set("disconnectReason", cond(closedByEvent, field("disconnectReason"), info.GetDisconnectReason().String()))
		f.set("closedBy", byEvent)
		room.set("internal.closedSids", union("internal.closedSids", sid))
		p.result.Cascades = append(p.result.Cascades,
			mediaCascades(sid, p.refs.ParticipantTracks[sid], p.ts, byEvent, p.retentionMs(schema.RetentionTracks))...)
	}

	p.upsert(schema.CollectionParticipants, sid, f)
	p.upsert(schema.CollectionRooms, roomSid, room)
}

// mediaCascades close, at the participant's end time, the tracks it still publishes, the subscriptions to the tracks
// it published and its own open subscriptions; LiveKit does not always emit their closing events.
func mediaCascades(sid string, published []string, at time.Time, closedBy string, tracksRetentionMs int64) []Update {
	expiresAt := at.Add(time.Duration(tracksRetentionMs) * time.Millisecond)

	tracks := newFields()
	tracks.set("status", trackUnpublished)
	tracks.set("unpublishedAt", at)
	tracks.set("closedBy", closedBy)
	tracks.set("durationMs", subtract(at, field("publishedAt")))
	tracks.set("expiresAt", expiresAt)
	cascades := []Update{{Collection: schema.CollectionTracks,
		Filter:   bson.D{{Key: "participantSid", Value: sid}, {Key: "status", Value: trackPublished}},
		Pipeline: []bson.D{stage(tracks.doc())}}}

	subscriptions := newFields()
	subscriptions.set("status", subscriptionUnsubscribed)
	subscriptions.set("unsubscribedAt", cond(exists("subscribedAt"), at, remove))
	subscriptions.set("closedBy", closedBy)
	subscriptions.set("durationMs", cond(exists("subscribedAt"), subtract(at, field("subscribedAt")), remove))
	subscriptions.set("expiresAt", expiresAt)
	open := bson.D{{Key: "$in", Value: bson.A{subscriptionRequested, subscriptionSubscribed}}}
	cascades = append(cascades, Update{Collection: schema.CollectionTrackSubscriptions,
		Filter:   bson.D{{Key: "subscriberSid", Value: sid}, {Key: "status", Value: open}},
		Pipeline: []bson.D{stage(subscriptions.doc())}})
	if len(published) > 0 {
		trackSids := bson.A{}
		for _, trackSid := range published {
			trackSids = append(trackSids, trackSid)
		}
		cascades = append(cascades, Update{Collection: schema.CollectionTrackSubscriptions,
			Filter:   bson.D{{Key: "trackSid", Value: bson.D{{Key: "$in", Value: trackSids}}}, {Key: "status", Value: open}},
			Pipeline: []bson.D{stage(subscriptions.doc())}})
	}
	return cascades
}

// joinTime reads ParticipantInfo.joined_at_ms, falling back to joined_at in seconds and then to the event time.
func joinTime(info *livekit.ParticipantInfo, fallback time.Time) time.Time {
	if info.GetJoinedAtMs() != 0 {
		return schema.UnixMillis(info.GetJoinedAtMs())
	}
	if info.GetJoinedAt() != 0 {
		return schema.UnixSeconds(info.GetJoinedAt())
	}
	return fallback
}

func clientDocument(client *livekit.ClientInfo, anonymizeIP bool) bson.D {
	document := bson.D{{Key: "sdk", Value: client.GetSdk().String()}}
	optional := []struct{ key, value string }{
		{"version", client.GetVersion()},
		{"os", client.GetOs()},
		{"osVersion", client.GetOsVersion()},
		{"browser", client.GetBrowser()},
		{"browserVersion", client.GetBrowserVersion()},
		{"deviceModel", client.GetDeviceModel()},
		{"network", client.GetNetwork()},
	}
	if !anonymizeIP {
		optional = append(optional, struct{ key, value string }{"address", client.GetAddress()})
	}
	for _, item := range optional {
		if item.value != "" {
			document = append(document, bson.E{Key: item.key, Value: item.value})
		}
	}
	return document
}

func permissionDocument(permission *livekit.ParticipantPermission) bson.D {
	document := bson.D{
		{Key: "canSubscribe", Value: permission.GetCanSubscribe()},
		{Key: "canPublish", Value: permission.GetCanPublish()},
		{Key: "canPublishData", Value: permission.GetCanPublishData()},
	}
	if sources := permission.GetCanPublishSources(); len(sources) > 0 {
		names := bson.A{}
		for _, source := range sources {
			names = append(names, source.String())
		}
		document = append(document, bson.E{Key: "canPublishSources", Value: names})
	}
	flags := []struct {
		key   string
		value bool
	}{
		{"hidden", permission.GetHidden()},
		{"canUpdateMetadata", permission.GetCanUpdateMetadata()},
		{"canSubscribeMetrics", permission.GetCanSubscribeMetrics()},
	}
	for _, flag := range flags {
		if flag.value {
			document = append(document, bson.E{Key: flag.key, Value: true})
		}
	}
	return document
}

func deriveParticipant(opts Options) *fields {
	retention, _, _ := opts.Retention.Of(schema.RetentionEntities)
	closed := in(field("status"), bson.A{participantLeft, participantAborted})

	f := newFields()
	f.set("resumes", sizeOf("internal.resumeEventIds"))
	f.set("isPublisher", keep("isPublisher", false))
	f.set("tracks.published", sizeOf("internal.trackSids"))
	f.set("tracks.bySource", sizesOf("internal.tracksBySource"))
	f.set("tracks.subscribed", sizeOf("internal.subscribedTrackSids"))
	f.set("tracks.subscribeFailures", sizeOf("internal.failedTrackSids"))
	f.set("bytes.up", keep("bytes.up", int64(0)))
	f.set("bytes.down", keep("bytes.down", int64(0)))
	setQualityDefaults(f)
	f.set("durationMs", cond(exists("leftAt"), subtract(field("leftAt"), field("joinedAt")), remove))
	f.set("expiresAt", cond(closed, add(field("leftAt"), retention.Milliseconds()), nil))
	return f
}
