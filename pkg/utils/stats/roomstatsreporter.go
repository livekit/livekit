package stats

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	roomTotal            int32
	participantTotal     int32
	trackPublishedTotal  int32
	trackSubscribedTotal int32

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

func initRoomStatsReporter() {
	prometheus.MustRegister(promRoomTotal)
	prometheus.MustRegister(promRoomDuration)
	prometheus.MustRegister(promParticipantTotal)
	prometheus.MustRegister(promTrackPublishedTotal)
	prometheus.MustRegister(promTrackSubscribedTotal)
}

// RoomStatsReporter is created for each room
type RoomStatsReporter struct {
	roomName  string
	startedAt time.Time
	Incoming  *PacketStats
	Outgoing  *PacketStats
}

func NewRoomStatsReporter(roomName string) *RoomStatsReporter {
	return &RoomStatsReporter{
		roomName: roomName,
		Incoming: newPacketStats(roomName, "incoming"),
		Outgoing: newPacketStats(roomName, "outgoing"),
	}
}

func (r *RoomStatsReporter) RoomStarted() {
	r.startedAt = time.Now()
	promRoomTotal.Add(1)
	atomic.AddInt32(&roomTotal, 1)
}

func (r *RoomStatsReporter) RoomEnded() {
	if !r.startedAt.IsZero() {
		promRoomDuration.Observe(float64(time.Now().Sub(r.startedAt)) / float64(time.Second))
	}
	promRoomTotal.Sub(1)
	atomic.AddInt32(&roomTotal, -1)
}

func (r *RoomStatsReporter) AddParticipant() {
	promParticipantTotal.Add(1)
	atomic.AddInt32(&participantTotal, 1)
}

func (r *RoomStatsReporter) SubParticipant() {
	promParticipantTotal.Sub(1)
	atomic.AddInt32(&participantTotal, -1)
}

func (r *RoomStatsReporter) AddPublishedTrack(kind string) {
	promTrackPublishedTotal.WithLabelValues(kind).Add(1)
	atomic.AddInt32(&trackPublishedTotal, 1)
}

func (r *RoomStatsReporter) SubPublishedTrack(kind string) {
	promTrackPublishedTotal.WithLabelValues(kind).Sub(1)
	atomic.AddInt32(&trackPublishedTotal, -1)
}

func (r *RoomStatsReporter) AddSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Add(1)
	atomic.AddInt32(&trackSubscribedTotal, 1)
}

func (r *RoomStatsReporter) SubSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Sub(1)
	atomic.AddInt32(&trackSubscribedTotal, -1)
}
