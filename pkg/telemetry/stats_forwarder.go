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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
)

// Self-host packet-loss timeline POC: forward per-participant audio stats to a
// disha endpoint so a user send/receive loss timeline can be reconstructed per
// call. See .cursor/plans/livekit_network_telemetry_poc.plan.md in disha-backend.
//
// This is deliberately scoped to the SendStats / SendEvent path so the fork stays
// small across LiveKit upgrades. The stock image leaves SendStats a no-op; this
// adds a side channel.
//
// Stats are NOT posted per flush. Each flush's audio points are buffered in-process,
// keyed by room, and the whole timeline is POSTed ONCE when the room ends
// (ROOM_ENDED), so the receiver gets a single payload per call.
//
// Both legs are forwarded: each point is labelled role "user" or "bot" (by AGENT
// participant kind) so the receiver can separate the user's phone leg from the
// bot<->SFU leg.

const (
	// Content type mirrors LiveKit webhook deliveries so the receiver can reuse the
	// same auth verification (signed JWT carrying the body sha256).
	forwardedStatsContentType = "application/webhook+json"

	// signedTokenTTL matches the webhook signer's validity window.
	signedTokenTTL = 5 * time.Minute

	// identityRetention keeps a departed participant's identity resolvable for the
	// post-close flush. It must exceed workerCleanupWait (3m) so the final flush
	// after a participant leaves can still be attributed.
	identityRetention = 5 * time.Minute

	// finalizeDelay waits past room end before posting the one-shot timeline. A
	// participant's final (partial) window is flushed on the first stats tick after
	// the room ends, i.e. up to one telemetryStatsUpdateInterval later, so we wait
	// that plus a margin to avoid dropping the last — often worst — loss window.
	finalizeDelay = telemetryStatsUpdateInterval + 15*time.Second

	defaultForwardTimeout = 5 * time.Second
)

// statForwardPoint is one coalesced audio stat window for one participant/direction.
// kind is "send" for UPSTREAM (user mic -> SFU) and "recv" for DOWNSTREAM (SFU ->
// user). rtt/jitter are forwarded raw (LiveKit's coalesced units, not seconds); the
// receiver converts after confirming units against a known call.
type statForwardPoint struct {
	ParticipantIdentity string  `json:"participant_identity"`
	ParticipantSID      string  `json:"participant_sid"`
	Role                string  `json:"role"` // "user" or "bot"
	Kind                string  `json:"kind"`
	Mime                string  `json:"mime,omitempty"`
	Time                string  `json:"time,omitempty"`
	StartTime           string  `json:"start_time,omitempty"`
	EndTime             string  `json:"end_time,omitempty"`
	Packets             uint32  `json:"packets"`
	PacketsLost         uint32  `json:"packets_lost"`
	PacketLossPct       float64 `json:"packet_loss_pct"`
	Rtt                 uint32  `json:"rtt"`
	Jitter              uint32  `json:"jitter"`
	Nacks               uint32  `json:"nacks"`
	Bytes               uint64  `json:"bytes"`
	BitrateBps          float64 `json:"bitrate_bps"`
	Score               float32 `json:"score"`
	MinScore            float32 `json:"min_score"`
	MedianScore         float32 `json:"median_score"`
}

type statForwardPayload struct {
	RoomName string             `json:"room_name"`
	RoomID   string             `json:"room_id,omitempty"`
	Node     string             `json:"node,omitempty"`
	Points   []statForwardPoint `json:"points"`
}

// roomBuffer accumulates a single call's user-audio points until the room ends.
type roomBuffer struct {
	roomName string
	roomID   string
	points   []statForwardPoint
}

type statsForwarder struct {
	url        string
	apiKey     string
	apiSecret  string
	nodeID     string
	timeout    time.Duration
	httpClient *http.Client

	identityMu sync.RWMutex
	identities map[livekit.ParticipantID]participantMeta

	buffersMu sync.Mutex
	buffers   map[string]*roomBuffer // keyed by room SID (fallback room name)
}

// participantMeta is what we need to attribute and classify a stat: the identity
// (AnalyticsStat only carries the SID) and the kind, used to drop the egress
// recorder leg.
type participantMeta struct {
	identity livekit.ParticipantIdentity
	kind     livekit.ParticipantInfo_Kind
}

// newStatsForwarder returns a forwarder, or nil when forwarding is not configured
// (no StatsURL) or cannot be authenticated (no signing key/secret). A nil forwarder
// means the analytics service keeps its stock no-op behavior.
func newStatsForwarder(acfg config.AnalyticsConfig, webhookAPIKey, nodeID string, keyProvider auth.KeyProvider) *statsForwarder {
	if acfg.StatsURL == "" {
		return nil
	}

	apiKey := acfg.APIKey
	if apiKey == "" {
		apiKey = webhookAPIKey
	}
	if apiKey == "" || keyProvider == nil {
		logger.Warnw("stats forwarding disabled: no API key available for signing", nil, "url", acfg.StatsURL)
		return nil
	}
	secret := keyProvider.GetSecret(apiKey)
	if secret == "" {
		logger.Warnw("stats forwarding disabled: no secret for API key", nil, "apiKey", apiKey)
		return nil
	}

	timeout := acfg.Timeout
	if timeout <= 0 {
		timeout = defaultForwardTimeout
	}

	logger.Infow("stats forwarding enabled", "url", acfg.StatsURL)
	return &statsForwarder{
		url:        acfg.StatsURL,
		apiKey:     apiKey,
		apiSecret:  secret,
		nodeID:     nodeID,
		timeout:    timeout,
		httpClient: &http.Client{Timeout: timeout},
		identities: make(map[livekit.ParticipantID]participantMeta),
		buffers:    make(map[string]*roomBuffer),
	}
}

// recordEvent maintains the participant SID -> identity map and triggers the
// one-shot post when a room ends. AnalyticsStat only carries the SID, so identity
// is resolved from the lifecycle events that already flow through SendEvent.
func (f *statsForwarder) recordEvent(event *livekit.AnalyticsEvent) {
	if event == nil {
		return
	}

	if event.Type == livekit.AnalyticsEventType_ROOM_ENDED {
		f.scheduleFinalize(roomKeyFromEvent(event))
		return
	}

	if event.Participant == nil {
		return
	}
	sid := livekit.ParticipantID(event.Participant.Sid)
	identity := livekit.ParticipantIdentity(event.Participant.Identity)
	if sid == "" || identity == "" {
		return
	}

	switch event.Type {
	case livekit.AnalyticsEventType_PARTICIPANT_JOINED,
		livekit.AnalyticsEventType_PARTICIPANT_ACTIVE,
		livekit.AnalyticsEventType_PARTICIPANT_RESUMED:
		f.identityMu.Lock()
		f.identities[sid] = participantMeta{identity: identity, kind: event.Participant.Kind}
		f.identityMu.Unlock()
	case livekit.AnalyticsEventType_PARTICIPANT_LEFT,
		livekit.AnalyticsEventType_PARTICIPANT_CONNECTION_ABORTED:
		// Defer removal so the post-close flush can still resolve identity.
		time.AfterFunc(identityRetention, func() {
			f.identityMu.Lock()
			delete(f.identities, sid)
			f.identityMu.Unlock()
		})
	}
}

func (f *statsForwarder) metaFor(sid livekit.ParticipantID) (participantMeta, bool) {
	f.identityMu.RLock()
	defer f.identityMu.RUnlock()
	meta, ok := f.identities[sid]
	return meta, ok
}

// forward filters a flush batch to audio stats and buffers them for the end-of-call
// post. It drops video, the egress recorder leg, and any stat whose participant
// identity cannot be resolved. Both the user and the bot leg are kept, each tagged
// with its role.
func (f *statsForwarder) forward(stats []*livekit.AnalyticsStat) {
	for _, stat := range stats {
		if !isAudioStat(stat) {
			continue
		}
		meta, ok := f.metaFor(livekit.ParticipantID(stat.ParticipantId))
		if !ok {
			// Can't attribute the stat to an identity yet; skip rather than guess.
			continue
		}
		if meta.kind == livekit.ParticipantInfo_EGRESS {
			// The recorder subscribes to tracks like a participant, but its leg is not
			// an audio-quality signal for the user or the bot.
			continue
		}
		// Classify by participant kind: disha mints the bot token with_kind("agent")
		// while the user stays STANDARD. (Identity can't be used: it is per-room,
		// bot-<room>.)
		role := "user"
		if meta.kind == livekit.ParticipantInfo_AGENT {
			role = "bot"
		}
		f.bufferPoint(stat, meta.identity, role)
	}
}

func (f *statsForwarder) bufferPoint(stat *livekit.AnalyticsStat, identity livekit.ParticipantIdentity, role string) {
	key := roomKey(stat.RoomId, stat.RoomName)
	if key == "" {
		return
	}
	point := buildStatPoint(stat, identity, role)

	f.buffersMu.Lock()
	defer f.buffersMu.Unlock()
	buf := f.buffers[key]
	if buf == nil {
		buf = &roomBuffer{roomName: stat.RoomName, roomID: stat.RoomId}
		f.buffers[key] = buf
	}
	buf.points = append(buf.points, point)
}

// scheduleFinalize posts the buffered timeline once, a short delay after the room
// ends, so the trailing post-close flush is captured.
func (f *statsForwarder) scheduleFinalize(key string) {
	if key == "" {
		return
	}
	time.AfterFunc(finalizeDelay, func() {
		f.finalize(key)
	})
}

func (f *statsForwarder) finalize(key string) {
	f.buffersMu.Lock()
	buf := f.buffers[key]
	delete(f.buffers, key)
	f.buffersMu.Unlock()

	if buf == nil || len(buf.points) == 0 {
		return
	}
	f.post(statForwardPayload{
		RoomName: buf.roomName,
		RoomID:   buf.roomID,
		Node:     f.nodeID,
		Points:   buf.points,
	})
}

func roomKey(roomID, roomName string) string {
	if roomID != "" {
		return roomID
	}
	return roomName
}

func roomKeyFromEvent(event *livekit.AnalyticsEvent) string {
	if event.RoomId != "" {
		return event.RoomId
	}
	if event.Room != nil {
		return roomKey(event.Room.Sid, event.Room.Name)
	}
	return ""
}

func isAudioStat(stat *livekit.AnalyticsStat) bool {
	return strings.HasPrefix(strings.ToLower(stat.Mime), "audio")
}

func buildStatPoint(stat *livekit.AnalyticsStat, identity livekit.ParticipantIdentity, role string) statForwardPoint {
	var primaryPackets, packetsLost, nacks, maxRtt, maxJitter uint32
	var totalBytes uint64
	var startTime, endTime time.Time

	for _, s := range stat.Streams {
		primaryPackets += s.PrimaryPackets
		packetsLost += s.PacketsLost
		nacks += s.Nacks
		totalBytes += s.PrimaryBytes + s.PaddingBytes + s.RetransmitBytes
		if s.Rtt > maxRtt {
			maxRtt = s.Rtt
		}
		if s.Jitter > maxJitter {
			maxJitter = s.Jitter
		}
		if s.StartTime != nil {
			st := s.StartTime.AsTime()
			if startTime.IsZero() || st.Before(startTime) {
				startTime = st
			}
		}
		if s.EndTime != nil {
			et := s.EndTime.AsTime()
			if et.After(endTime) {
				endTime = et
			}
		}
	}

	// Same formula as Daily remoteInbound-packetLossPct: lost / (received + lost).
	pct := 0.0
	if denom := primaryPackets + packetsLost; denom > 0 {
		pct = 100 * float64(packetsLost) / float64(denom)
	}

	bitrateBps := 0.0
	if !startTime.IsZero() && endTime.After(startTime) {
		bitrateBps = float64(totalBytes) * 8 / endTime.Sub(startTime).Seconds()
	}

	kind := "recv"
	if stat.Kind == livekit.StreamType_UPSTREAM {
		kind = "send"
	}

	point := statForwardPoint{
		ParticipantIdentity: string(identity),
		ParticipantSID:      stat.ParticipantId,
		Role:                role,
		Kind:                kind,
		Mime:                stat.Mime,
		Packets:             primaryPackets,
		PacketsLost:         packetsLost,
		PacketLossPct:       pct,
		Rtt:                 maxRtt,
		Jitter:              maxJitter,
		Nacks:               nacks,
		Bytes:               totalBytes,
		BitrateBps:          bitrateBps,
		Score:               stat.Score,
		MinScore:            stat.MinScore,
		MedianScore:         stat.MedianScore,
	}
	if stat.TimeStamp != nil {
		point.Time = stat.TimeStamp.AsTime().UTC().Format(time.RFC3339Nano)
	}
	if !startTime.IsZero() {
		point.StartTime = startTime.UTC().Format(time.RFC3339Nano)
	}
	if !endTime.IsZero() {
		point.EndTime = endTime.UTC().Format(time.RFC3339Nano)
	}
	return point
}

func (f *statsForwarder) post(payload statForwardPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warnw("failed to marshal forwarded stats", err)
		return
	}

	token, err := f.signToken(body)
	if err != nil {
		logger.Warnw("failed to sign forwarded stats", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(body))
	if err != nil {
		logger.Warnw("failed to build forwarded stats request", err)
		return
	}
	req.Header.Set("Content-Type", forwardedStatsContentType)
	req.Header.Set("Authorization", token)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		logger.Warnw("failed to post forwarded stats", err, "url", f.url)
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusMultipleChoices {
		logger.Warnw("forwarded stats endpoint returned error", nil, "status", resp.StatusCode, "url", f.url)
	}
}

// signToken mirrors LiveKit's webhook signing: a JWT carrying the base64 sha256 of
// the body, signed with the API key/secret. The receiver verifies it the same way
// it verifies webhooks (TokenVerifier + body hash check).
func (f *statsForwarder) signToken(body []byte) (string, error) {
	sum := sha256.Sum256(body)
	hash := base64.StdEncoding.EncodeToString(sum[:])
	return auth.NewAccessToken(f.apiKey, f.apiSecret).
		SetValidFor(signedTokenTTL).
		SetSha256(hash).
		ToJWT()
}
