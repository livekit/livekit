package prometheus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
)

var (
	roomTotal              atomic.Int32
	participantTotal       atomic.Int32
	trackPublishedTotal    atomic.Int32
	trackSubscribedTotal   atomic.Int32
	trackPublishAttempts   atomic.Int32
	trackPublishSuccess    atomic.Int32
	trackSubscribeAttempts atomic.Int32
	trackSubscribeSuccess  atomic.Int32

	promRoomTotal             prometheus.Gauge
	promRoomDuration          prometheus.Histogram
	promParticipantTotal      prometheus.Gauge
	promTrackPublishedTotal   *prometheus.GaugeVec
	promTrackSubscribedTotal  *prometheus.GaugeVec
	promTrackPublishCounter   *prometheus.CounterVec
	promTrackSubscribeCounter *prometheus.CounterVec
)

func initRoomStats(nodeID string, nodeType livekit.NodeType) {
	promRoomTotal = prometheus.NewGauge(prometheus.GaugeOpts{
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
	promParticipantTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "participant",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	})
	promTrackPublishedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "published_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"kind"})
	promTrackSubscribedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "subscribed_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"kind"})
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
	}, []string{"kind", "state"})

	prometheus.MustRegister(promRoomTotal)
	prometheus.MustRegister(promRoomDuration)
	prometheus.MustRegister(promParticipantTotal)
	prometheus.MustRegister(promTrackPublishedTotal)
	prometheus.MustRegister(promTrackSubscribedTotal)
	prometheus.MustRegister(promTrackPublishCounter)
	prometheus.MustRegister(promTrackSubscribeCounter)
}

func RoomStarted() {
	promRoomTotal.Add(1)
	roomTotal.Inc()
}

func RoomEnded(startedAt time.Time) {
	if !startedAt.IsZero() {
		promRoomDuration.Observe(float64(time.Since(startedAt)) / float64(time.Second))
	}
	promRoomTotal.Sub(1)
	roomTotal.Dec()
}

func AddParticipant() {
	promParticipantTotal.Add(1)
	participantTotal.Inc()
}

func SubParticipant() {
	promParticipantTotal.Sub(1)
	participantTotal.Dec()
}

func AddPublishedTrack(kind string) {
	promTrackPublishedTotal.WithLabelValues(kind).Add(1)
	trackPublishedTotal.Inc()
}

func SubPublishedTrack(kind string) {
	promTrackPublishedTotal.WithLabelValues(kind).Sub(1)
	trackPublishedTotal.Dec()
}

func AddPublishAttempt(kind string, state string) {
	promTrackPublishCounter.WithLabelValues(kind, state).Inc()
	trackPublishAttempts.Inc()
	if state == "success" {
		trackPublishSuccess.Inc()
	}
}

func AddSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Add(1)
	trackSubscribedTotal.Inc()
}

func SubSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Sub(1)
	trackSubscribedTotal.Dec()
}

func AddSubscribeAttempt(kind string, success bool) {
	trackSubscribeAttempts.Inc()
	if success {
		promTrackSubscribeCounter.WithLabelValues(kind, "success").Inc()
		trackSubscribeSuccess.Inc()
	} else {
		promTrackSubscribeCounter.WithLabelValues(kind, "attempt").Inc()
	}
}
