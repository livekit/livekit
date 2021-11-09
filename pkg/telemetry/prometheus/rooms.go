package prometheus

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	atomicRoomTotal            int32
	atomicParticipantTotal     int32
	atomicTrackPublishedTotal  int32
	atomicTrackSubscribedTotal int32

	promRoomTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: livekitNamespace,
		Subsystem: "room",
		Name:      "total",
	})
	promRoomDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: livekitNamespace,
		Subsystem: "room",
		Name:      "duration_seconds",
		Buckets: []float64{
			5, 10, 60, 5 * 60, 10 * 60, 30 * 60, 60 * 60, 2 * 60 * 60, 5 * 60 * 60, 10 * 60 * 60,
		},
	})
	promParticipantTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: livekitNamespace,
		Subsystem: "participant",
		Name:      "total",
	})
	promTrackPublishedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: livekitNamespace,
		Subsystem: "track",
		Name:      "published_total",
	}, []string{"kind"})
	promTrackSubscribedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: livekitNamespace,
		Subsystem: "track",
		Name:      "subscribed_total",
	}, []string{"kind"})
)

func initRoomStats() {
	prometheus.MustRegister(promRoomTotal)
	prometheus.MustRegister(promRoomDuration)
	prometheus.MustRegister(promParticipantTotal)
	prometheus.MustRegister(promTrackPublishedTotal)
	prometheus.MustRegister(promTrackSubscribedTotal)
}

func RoomStarted() {
	promRoomTotal.Add(1)
	atomic.AddInt32(&atomicRoomTotal, 1)
}

func RoomEnded(startedAt time.Time) {
	if !startedAt.IsZero() {
		promRoomDuration.Observe(float64(time.Now().Sub(startedAt)) / float64(time.Second))
	}
	promRoomTotal.Sub(1)
	atomic.AddInt32(&atomicRoomTotal, -1)
}

func AddParticipant() {
	promParticipantTotal.Add(1)
	atomic.AddInt32(&atomicParticipantTotal, 1)
}

func SubParticipant() {
	promParticipantTotal.Sub(1)
	atomic.AddInt32(&atomicParticipantTotal, -1)
}

func AddPublishedTrack(kind string) {
	promTrackPublishedTotal.WithLabelValues(kind).Add(1)
	atomic.AddInt32(&atomicTrackPublishedTotal, 1)
}

func SubPublishedTrack(kind string) {
	promTrackPublishedTotal.WithLabelValues(kind).Sub(1)
	atomic.AddInt32(&atomicTrackPublishedTotal, -1)
}

func AddSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Add(1)
	atomic.AddInt32(&atomicTrackSubscribedTotal, 1)
}

func SubSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Sub(1)
	atomic.AddInt32(&atomicTrackSubscribedTotal, -1)
}
