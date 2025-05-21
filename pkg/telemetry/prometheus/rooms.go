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
	trackSubscribeAttempts atomic.Int32
	trackSubscribeSuccess  atomic.Int32
	// count the number of failures that are due to user error (permissions, track doesn't exist), so we could compute
	// success rate by subtracting this from total attempts
	trackSubscribeUserError atomic.Int32

	promRoomCurrent            prometheus.Gauge
	promRoomDuration           prometheus.Histogram
	promParticipantCurrent     prometheus.Gauge
	promTrackPublishedCurrent  *prometheus.GaugeVec
	promTrackSubscribedCurrent *prometheus.GaugeVec
	promTrackPublishCounter    *prometheus.CounterVec
	promTrackSubscribeCounter  *prometheus.CounterVec
	promSessionStartTime       *prometheus.HistogramVec
	promSessionDuration        *prometheus.HistogramVec
	promPubSubTime             *prometheus.HistogramVec
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
	}, []string{"kind", "source", "mimeType"})
	promTrackSubscribedCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "subscribed_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"kind", "source", "mimeType"})
	promTrackPublishCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "publish_counter",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"kind", "source", "mimeType", "state"})
	promTrackSubscribeCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "subscribe_counter",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"state", "error"})
	promSessionStartTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "session",
		Name:        "start_time_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     prometheus.ExponentialBucketsRange(100, 10000, 15),
	}, []string{"protocol_version"})
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

	prometheus.MustRegister(promRoomCurrent)
	prometheus.MustRegister(promRoomDuration)
	prometheus.MustRegister(promParticipantCurrent)
	prometheus.MustRegister(promTrackPublishedCurrent)
	prometheus.MustRegister(promTrackSubscribedCurrent)
	prometheus.MustRegister(promTrackPublishCounter)
	prometheus.MustRegister(promTrackSubscribeCounter)
	prometheus.MustRegister(promSessionStartTime)
	prometheus.MustRegister(promSessionDuration)
	prometheus.MustRegister(promPubSubTime)
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

func AddPublishedTrack(kind, source, mimeType string) {
	promTrackPublishedCurrent.WithLabelValues(kind, source, mimeType).Add(1)
	trackPublishedCurrent.Inc()
}

func SubPublishedTrack(kind, source, mimeType string) {
	promTrackPublishedCurrent.WithLabelValues(kind, source, mimeType).Sub(1)
	trackPublishedCurrent.Dec()
}

func AddPublishAttempt(kind, source, mimeType string) {
	trackPublishAttempts.Inc()
	promTrackPublishCounter.WithLabelValues(kind, source, mimeType, "attempt").Inc()
}

func AddPublishSuccess(kind, source, mimeType string) {
	trackPublishSuccess.Inc()
	promTrackPublishCounter.WithLabelValues(kind, source, mimeType, "success").Inc()
}

func RecordPublishTime(source livekit.TrackSource, trackType livekit.TrackType, d time.Duration, sdk livekit.ClientInfo_SDK, kind livekit.ParticipantInfo_Kind) {
	recordPubSubTime(true, source, trackType, d, sdk, kind, 1)
}

func RecordSubscribeTime(source livekit.TrackSource, trackType livekit.TrackType, d time.Duration, sdk livekit.ClientInfo_SDK, kind livekit.ParticipantInfo_Kind, count int) {
	recordPubSubTime(false, source, trackType, d, sdk, kind, count)
}

func recordPubSubTime(isPublish bool, source livekit.TrackSource, trackType livekit.TrackType, d time.Duration, sdk livekit.ClientInfo_SDK, kind livekit.ParticipantInfo_Kind, count int) {
	direction := "subscribe"
	if isPublish {
		direction = "publish"
	}
	promPubSubTime.WithLabelValues(direction, source.String(), trackType.String(), sdk.String(), kind.String(), strconv.Itoa(count)).Observe(float64(d.Milliseconds()))
}

func RecordTrackSubscribeSuccess(kind, source, mimeType string) {
	// modify both current and total counters
	promTrackSubscribedCurrent.WithLabelValues(kind, source, mimeType).Add(1)
	trackSubscribedCurrent.Inc()

	promTrackSubscribeCounter.WithLabelValues("success", "").Inc()
	trackSubscribeSuccess.Inc()
}

func RecordTrackUnsubscribed(kind, source, mimeType string) {
	// unsubscribed modifies current counter, but we leave the total values alone since they
	// are used to compute rate
	promTrackSubscribedCurrent.WithLabelValues(kind, source, mimeType).Sub(1)
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
	}
}

func RecordSessionStartTime(protocolVersion int, d time.Duration) {
	promSessionStartTime.WithLabelValues(strconv.Itoa(protocolVersion)).Observe(float64(d.Milliseconds()))
}

func RecordSessionDuration(protocolVersion int, d time.Duration) {
	promSessionDuration.WithLabelValues(strconv.Itoa(protocolVersion)).Observe(float64(d.Milliseconds()))
}
