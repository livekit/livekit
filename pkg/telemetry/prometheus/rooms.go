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

package prometheus

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
)

var (
	roomCurrent            atomic.Int32
	participantCurrent     atomic.Int32
	trackPublishedCurrent  atomic.Int32
	trackSubscribedCurrent atomic.Int32
	trackPublishAttempts   atomic.Int32
	trackPublishSuccess    atomic.Int32
	trackPublishCancels    atomic.Int32
	trackSubscribeAttempts atomic.Int32
	trackSubscribeSuccess  atomic.Int32
	trackSubscribeCancels  atomic.Int32
	// count the number of failures that are due to user error (permissions, track doesn't exist), so we could compute
	// success rate by subtracting this from total attempts
	trackSubscribeUserError atomic.Int32

	promRoomCurrent                        prometheus.Gauge
	promRoomDuration                       prometheus.Histogram
	promParticipantCurrent                 prometheus.Gauge
	promTrackPublishedCurrent              *prometheus.GaugeVec
	promTrackSubscribedCurrent             *prometheus.GaugeVec
	promTrackPacketTrailerCurrent          prometheus.Gauge
	promTrackPacketTrailerByFeatureCurrent *prometheus.GaugeVec
	promTrackPublishCounter                *prometheus.CounterVec
	promTrackSubscribeCounter              *prometheus.CounterVec
	promSessionJoinLatency                 *prometheus.HistogramVec
	promSessionStartTime                   *prometheus.HistogramVec
	promSessionOfferToAnswer               *prometheus.HistogramVec
	promSessionAnswerToICE                 *prometheus.HistogramVec
	promSessionICEToDTLS                   *prometheus.HistogramVec
	promSessionICEToActive                 *prometheus.HistogramVec
	promSessionJoinToOffer                 *prometheus.HistogramVec
	promSessionJoinPathEvent               *prometheus.CounterVec
	promSessionDuration                    *prometheus.HistogramVec
	promPubSubTime                         *prometheus.HistogramVec
	promSubscribePaceShadow                *prometheus.HistogramVec

	promPeerConnection *prometheus.CounterVec

	// ICE step 5 (connectivity checks) + step 6 (nomination) — FK deep-dive.
	promICEStep5CheckToNominate     *prometheus.HistogramVec
	promICEStep6NominateToConnected *prometheus.HistogramVec
	promICEStep56CheckToConnected   *prometheus.HistogramVec
)

func initRoomStats(nodeID string, nodeType livekit.NodeType) {
	promRoomCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "room",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	})
	promRoomDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "room",
		Name:        "duration_seconds",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets: []float64{
			5, 10, 60, 5 * 60, 10 * 60, 30 * 60, 60 * 60, 2 * 60 * 60, 5 * 60 * 60, 10 * 60 * 60,
		},
	})
	promParticipantCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "participant",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	})
	promTrackPublishedCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "published_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"kind"})
	promTrackSubscribedCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "subscribed_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"kind"})
	promTrackPacketTrailerCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "packet_trailer_total",
		Help:        "Current number of published video tracks with packet trailers enabled.",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	})
	promTrackPacketTrailerByFeatureCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "packet_trailer_feature_total",
		Help:        "Current number of published video tracks by enabled packet trailer feature.",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"feature"})
	for value := range livekit.PacketTrailerFeature_name {
		feature := livekit.PacketTrailerFeature(value)
		promTrackPacketTrailerByFeatureCurrent.WithLabelValues(feature.String()).Set(0)
	}
	promTrackPublishCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "publish_counter",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"kind", "state"})
	promTrackSubscribeCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "subscribe_counter",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"state", "error"})
	promSessionJoinLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "join_latency_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     prometheus.ExponentialBucketsRange(10, 10000, 15),
	}, []string{"protocol_version"})
	promSessionStartTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "start_time_ms",
		Help:        "Session start → participant ACTIVE. Tagged with ICE path when known.",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     prometheus.ExponentialBucketsRange(100, 10000, 15),
	}, []string{"protocol_version", "warp", "sdk", "local_candidate_type", "remote_candidate_type", "connection_type"})
	// Join-path stage budgets (phone p95 < 300ms needs all three under ~120/150/80).
	sessionStageBuckets := prometheus.ExponentialBucketsRange(10, 10000, 15)
	promSessionOfferToAnswer = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "offer_to_answer_ms",
		Help:        "Subscriber offer sent → answer received (SDP + signaling RTT).",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     sessionStageBuckets,
	}, []string{"protocol_version", "sdk"})
	// Post-ICE session stages carry candidate/connection tags for TURN vs UDP deep-dives.
	sessionICELabels := []string{"protocol_version", "sdk", "local_candidate_type", "remote_candidate_type", "connection_type"}
	promSessionAnswerToICE = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "answer_to_ice_ms",
		Help:        "Subscriber answer received → primary ICE connected (TURN/ICE).",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     sessionStageBuckets,
	}, sessionICELabels)
	promSessionICEToDTLS = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "ice_to_dtls_ms",
		Help:        "ICE connected → PeerConnection connected (DTLS handshake). Labeled by transport (PUBLISHER/SUBSCRIBER).",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     sessionStageBuckets,
	}, append([]string{"protocol_version", "sdk", "transport"}, "local_candidate_type", "remote_candidate_type", "connection_type"))
	promSessionICEToActive = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "ice_to_active_ms",
		Help:        "Primary ICE connected → participant ACTIVE (includes DTLS on primary PC).",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     sessionStageBuckets,
	}, sessionICELabels)
	promSessionJoinToOffer = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "join_to_offer_ms",
		Help:        "Session start → first subscriber offer sent.",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     sessionStageBuckets,
	}, []string{"protocol_version", "sdk"})
	// Diagnostic counters for join-path shortcomings (FK fork).
	// event values: deferred_negotiate, renegotiate_after_answer, sub_offer_after_answer,
	// faststart_empty_tracks, faststart_with_tracks, active_resubscribe_skip, active_first_subscribe
	promSessionJoinPathEvent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "join_path_event_total",
		Help:        "Join-path diagnostic events for latency deep-dives.",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"event", "protocol_version", "sdk"})
	promSessionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "duration_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     prometheus.ExponentialBucketsRange(100, 4*60*60*1000, 15),
	}, []string{"protocol_version"})
	promPubSubTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "pubsubtime",
		Name:        "ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     []float64{100, 200, 500, 700, 1000, 5000, 10000},
	}, append(promStreamLabels, "sdk", "kind", "count"))
	promSubscribePaceShadow = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "subscribe_pace",
		Name:        "shadow_delay_ms",
		Help:        "Wait that pacing video subscribes at each rate would add, per video subscribe.",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     []float64{0, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000},
	}, []string{"rate"})
	promPeerConnection = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "peer_connection",
		Name:        "state",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"transport", "state"})

	// ICE step budgets: Checking → nominated pair → ICE Connected.
	iceStepBuckets := prometheus.ExponentialBucketsRange(5, 5000, 15)
	iceStepLabels := []string{"transport", "local_candidate_type", "remote_candidate_type", "connection_type"}
	promICEStep5CheckToNominate = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "ice",
		Name:        "step5_check_to_nominate_ms",
		Help:        "Step 5: ICE Checking started → first nominated/selected candidate pair.",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     iceStepBuckets,
	}, iceStepLabels)
	promICEStep6NominateToConnected = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "ice",
		Name:        "step6_nominate_to_connected_ms",
		Help:        "Step 6: first nominated/selected pair → ICE Connected.",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     iceStepBuckets,
	}, iceStepLabels)
	promICEStep56CheckToConnected = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "ice",
		Name:        "step5_6_check_to_connected_ms",
		Help:        "Steps 5+6: ICE Checking started → ICE Connected (full SFU-side ICE).",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     iceStepBuckets,
	}, iceStepLabels)

	prometheus.MustRegister(promRoomCurrent)
	prometheus.MustRegister(promRoomDuration)
	prometheus.MustRegister(promParticipantCurrent)
	prometheus.MustRegister(promTrackPublishedCurrent)
	prometheus.MustRegister(promTrackSubscribedCurrent)
	prometheus.MustRegister(promTrackPacketTrailerCurrent)
	prometheus.MustRegister(promTrackPacketTrailerByFeatureCurrent)
	prometheus.MustRegister(promTrackPublishCounter)
	prometheus.MustRegister(promTrackSubscribeCounter)
	prometheus.MustRegister(promSessionJoinLatency)
	prometheus.MustRegister(promSessionStartTime)
	prometheus.MustRegister(promSessionOfferToAnswer)
	prometheus.MustRegister(promSessionAnswerToICE)
	prometheus.MustRegister(promSessionICEToDTLS)
	prometheus.MustRegister(promSessionICEToActive)
	prometheus.MustRegister(promSessionJoinToOffer)
	prometheus.MustRegister(promSessionJoinPathEvent)
	prometheus.MustRegister(promSessionDuration)
	prometheus.MustRegister(promPubSubTime)
	prometheus.MustRegister(promPeerConnection)
	prometheus.MustRegister(promSubscribePaceShadow)
	prometheus.MustRegister(promICEStep5CheckToNominate)
	prometheus.MustRegister(promICEStep6NominateToConnected)
	prometheus.MustRegister(promICEStep56CheckToConnected)
}

func RoomStarted() {
	promRoomCurrent.Add(1)
	roomCurrent.Inc()
}

func RoomEnded(startedAt time.Time) {
	if !startedAt.IsZero() {
		promRoomDuration.Observe(float64(time.Since(startedAt)) / float64(time.Second))
	}
	promRoomCurrent.Sub(1)
	roomCurrent.Dec()
}

func AddParticipant() {
	promParticipantCurrent.Add(1)
	participantCurrent.Inc()
}

func SubParticipant() {
	promParticipantCurrent.Sub(1)
	participantCurrent.Dec()
}

func AddPublishedTrack(kind string) {
	promTrackPublishedCurrent.WithLabelValues(kind).Add(1)
	trackPublishedCurrent.Inc()
}

func SubPublishedTrack(kind string) {
	promTrackPublishedCurrent.WithLabelValues(kind).Sub(1)
	trackPublishedCurrent.Dec()
}

func AddPacketTrailerTrack(track *livekit.TrackInfo) {
	updatePacketTrailerTracks(track, 1)
}

func SubPacketTrailerTrack(track *livekit.TrackInfo) {
	updatePacketTrailerTracks(track, -1)
}

func updatePacketTrailerTracks(track *livekit.TrackInfo, delta float64) {
	if track.GetType() != livekit.TrackType_VIDEO {
		return
	}

	features := packetTrailerFeatureLabels(track.GetPacketTrailerFeatures())
	if len(features) == 0 {
		return
	}

	promTrackPacketTrailerCurrent.Add(delta)
	for _, feature := range features {
		promTrackPacketTrailerByFeatureCurrent.WithLabelValues(feature).Add(delta)
	}
}

func packetTrailerFeatureLabels(features []livekit.PacketTrailerFeature) []string {
	labels := make([]string, 0, len(features))
	seen := make(map[string]struct{}, len(features))
	for _, feature := range features {
		label := "UNKNOWN"
		if _, ok := livekit.PacketTrailerFeature_name[int32(feature)]; ok {
			label = feature.String()
		}

		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func RecordTrackPublishAttempt(kind string) {
	trackPublishAttempts.Inc()
	promTrackPublishCounter.WithLabelValues(kind, "attempt").Inc()
}

func RecordTrackPublishSuccess(kind string) {
	trackPublishSuccess.Inc()
	promTrackPublishCounter.WithLabelValues(kind, "success").Inc()
}

func RecordTrackPublishCancels(kind string, numCancels int32) {
	trackPublishCancels.Add(numCancels)
	promTrackPublishCounter.WithLabelValues(kind, "cancel").Add(float64(numCancels))
}

func RecordPublishTime(
	country string,
	source livekit.TrackSource,
	trackType livekit.TrackType,
	d time.Duration,
	sdk livekit.ClientInfo_SDK,
	kind livekit.ParticipantInfo_Kind,
) {
	recordPubSubStreamStartTime("publish", country, source, trackType, d, sdk, kind, 1)
}

func RecordSubscribeTime(
	country string,
	source livekit.TrackSource,
	trackType livekit.TrackType,
	d time.Duration,
	sdk livekit.ClientInfo_SDK,
	kind livekit.ParticipantInfo_Kind,
	count int,
) {
	recordPubSubStreamStartTime("subscribe", country, source, trackType, d, sdk, kind, count)
}

func RecordSubscribeStreamStartTime(
	country string,
	source livekit.TrackSource,
	trackType livekit.TrackType,
	d time.Duration,
	sdk livekit.ClientInfo_SDK,
	kind livekit.ParticipantInfo_Kind,
) {
	recordPubSubStreamStartTime("subscribe-stream-start", country, source, trackType, d, sdk, kind, 1)
}

func recordPubSubStreamStartTime(
	direction string,
	country string,
	source livekit.TrackSource,
	trackType livekit.TrackType,
	d time.Duration,
	sdk livekit.ClientInfo_SDK,
	kind livekit.ParticipantInfo_Kind,
	count int,
) {
	promPubSubTime.WithLabelValues(
		direction,
		source.String(),
		trackType.String(),
		country,
		sdk.String(),
		kind.String(),
		strconv.Itoa(count),
	).Observe(float64(d.Milliseconds()))
}

// RecordSubscribePaceShadow records, per shadow pacing rate, the wait one video subscribe would have had.
func RecordSubscribePaceShadow(delays map[string]time.Duration) {
	for rate, delay := range delays {
		promSubscribePaceShadow.WithLabelValues(rate).Observe(float64(delay) / float64(time.Millisecond))
	}
}

func RecordTrackSubscribeSuccess(kind string) {
	// modify both current and total counters
	promTrackSubscribedCurrent.WithLabelValues(kind).Add(1)
	trackSubscribedCurrent.Inc()

	promTrackSubscribeCounter.WithLabelValues("success", "").Inc()
	trackSubscribeSuccess.Inc()
}

func RecordTrackUnsubscribed(kind string) {
	// unsubscribed modifies current counter, but we leave the total values alone since they
	// are used to compute rate
	promTrackSubscribedCurrent.WithLabelValues(kind).Sub(1)
	trackSubscribedCurrent.Dec()
}

func RecordTrackSubscribeAttempt() {
	trackSubscribeAttempts.Inc()
	promTrackSubscribeCounter.WithLabelValues("attempt", "").Inc()
}

func RecordTrackSubscribeFailure(err error, isUserError bool) {
	promTrackSubscribeCounter.WithLabelValues("failure", err.Error()).Inc()

	if isUserError {
		trackSubscribeUserError.Inc()
		trackSubscribeCancels.Inc()
	}
}

func RecordTrackSubscribeCancels(numCancels int32) {
	trackSubscribeCancels.Add(numCancels)
	promTrackSubscribeCounter.WithLabelValues("cancel", "").Add(float64(numCancels))
}

func RecordSessionJoinLatency(protocolVersion int, d time.Duration) {
	promSessionJoinLatency.WithLabelValues(strconv.Itoa(protocolVersion)).Observe(float64(d.Milliseconds()))
}

func RecordSessionStartTime(
	protocolVersion int, warp bool, sdk livekit.ClientInfo_SDK, d time.Duration,
	localType, remoteType, connectionType string,
) {
	promSessionStartTime.WithLabelValues(
		strconv.Itoa(protocolVersion), strconv.FormatBool(warp), sdk.String(),
		labelOrUnknown(localType), labelOrUnknown(remoteType), labelOrUnknown(connectionType),
	).Observe(float64(d.Milliseconds()))
}

func RecordSessionOfferToAnswer(protocolVersion int, sdk livekit.ClientInfo_SDK, d time.Duration) {
	if d < 0 {
		return
	}
	promSessionOfferToAnswer.WithLabelValues(strconv.Itoa(protocolVersion), sdk.String()).Observe(float64(d.Milliseconds()))
}

func RecordSessionAnswerToICE(
	protocolVersion int, sdk livekit.ClientInfo_SDK, d time.Duration,
	localType, remoteType, connectionType string,
) {
	if d < 0 {
		return
	}
	promSessionAnswerToICE.WithLabelValues(
		strconv.Itoa(protocolVersion), sdk.String(),
		labelOrUnknown(localType), labelOrUnknown(remoteType), labelOrUnknown(connectionType),
	).Observe(float64(d.Milliseconds()))
}

// RecordSessionICEToDTLS records ICE Connected → PeerConnection Connected (DTLS done).
// transport is PUBLISHER or SUBSCRIBER (SignalTarget.String()).
func RecordSessionICEToDTLS(
	protocolVersion int, sdk livekit.ClientInfo_SDK, transport string, d time.Duration,
	localType, remoteType, connectionType string,
) {
	if d < 0 {
		return
	}
	promSessionICEToDTLS.WithLabelValues(
		strconv.Itoa(protocolVersion), sdk.String(), transport,
		labelOrUnknown(localType), labelOrUnknown(remoteType), labelOrUnknown(connectionType),
	).Observe(float64(d.Milliseconds()))
}

func RecordSessionICEToActive(
	protocolVersion int, sdk livekit.ClientInfo_SDK, d time.Duration,
	localType, remoteType, connectionType string,
) {
	if d < 0 {
		return
	}
	promSessionICEToActive.WithLabelValues(
		strconv.Itoa(protocolVersion), sdk.String(),
		labelOrUnknown(localType), labelOrUnknown(remoteType), labelOrUnknown(connectionType),
	).Observe(float64(d.Milliseconds()))
}

func RecordSessionJoinToOffer(protocolVersion int, sdk livekit.ClientInfo_SDK, d time.Duration) {
	if d < 0 {
		return
	}
	promSessionJoinToOffer.WithLabelValues(strconv.Itoa(protocolVersion), sdk.String()).Observe(float64(d.Milliseconds()))
}

// RecordSessionJoinPathEvent increments a diagnostic counter.
// Known events: deferred_negotiate, renegotiate_after_answer, sub_offer_after_answer,
// faststart_empty_tracks, faststart_with_tracks, active_resubscribe_skip, active_first_subscribe.
func RecordSessionJoinPathEvent(event string, protocolVersion int, sdk livekit.ClientInfo_SDK) {
	promSessionJoinPathEvent.WithLabelValues(event, strconv.Itoa(protocolVersion), sdk.String()).Inc()
}

func RecordSessionDuration(protocolVersion int, d time.Duration) {
	promSessionDuration.WithLabelValues(strconv.Itoa(protocolVersion)).Observe(float64(d.Milliseconds()))
}

func RecordPeerConnectionState(transport livekit.SignalTarget, state string) {
	promPeerConnection.WithLabelValues(transport.String(), state).Inc()
}

// RecordICEStepLatencies records SFU-side ICE step 5/6 durations.
// localType/remoteType are webrtc ICE candidate types (host/srflx/relay/prflx).
// connectionType is LiveKit ICEConnectionType (udp/tcp/turn/…).
func RecordICEStepLatencies(
	transport livekit.SignalTarget,
	localType, remoteType, connectionType string,
	checkToNominate, nominateToConnected, checkToConnected time.Duration,
) {
	labels := []string{transport.String(), labelOrUnknown(localType), labelOrUnknown(remoteType), labelOrUnknown(connectionType)}
	if checkToNominate >= 0 {
		promICEStep5CheckToNominate.WithLabelValues(labels...).Observe(float64(checkToNominate.Milliseconds()))
	}
	if nominateToConnected >= 0 {
		promICEStep6NominateToConnected.WithLabelValues(labels...).Observe(float64(nominateToConnected.Milliseconds()))
	}
	if checkToConnected >= 0 {
		promICEStep56CheckToConnected.WithLabelValues(labels...).Observe(float64(checkToConnected.Milliseconds()))
	}
}

func labelOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
