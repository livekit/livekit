package prometheus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"
)

var (
	roomTotal            atomic.Int32
	participantTotal     atomic.Int32
	trackPublishedTotal  atomic.Int32
	trackSubscribedTotal atomic.Int32

	promRoomTotal            prometheus.Gauge
	promRoomDuration         prometheus.Histogram
	promParticipantTotal     prometheus.Gauge
	promTrackPublishedTotal  *prometheus.GaugeVec
	promTrackSubscribedTotal *prometheus.GaugeVec
)

func initRoomStats(nodeID string) {
	promRoomTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "room",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	})
	promRoomDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "room",
		Name:        "duration_seconds",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
		Buckets: []float64{
			5, 10, 60, 5 * 60, 10 * 60, 30 * 60, 60 * 60, 2 * 60 * 60, 5 * 60 * 60, 10 * 60 * 60,
		},
	})
	promParticipantTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "participant",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	})
	promTrackPublishedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "published_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, []string{"kind"})
	promTrackSubscribedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "track",
		Name:        "subscribed_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, []string{"kind"})

	prometheus.MustRegister(promRoomTotal)
	prometheus.MustRegister(promRoomDuration)
	prometheus.MustRegister(promParticipantTotal)
	prometheus.MustRegister(promTrackPublishedTotal)
	prometheus.MustRegister(promTrackSubscribedTotal)
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

func AddSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Add(1)
	trackSubscribedTotal.Inc()
}

func SubSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Sub(1)
	trackSubscribedTotal.Dec()
}
