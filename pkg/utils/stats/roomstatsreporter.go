package stats

import (
	"sync/atomic"
	"time"

	livekit "github.com/livekit/protocol/proto"
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

func NewRoomStatsReporter() *RoomStatsReporter {
	return &RoomStatsReporter{
		Incoming: newPacketStats("incoming"),
		Outgoing: newPacketStats("outgoing"),
	}
}

func (r *RoomStatsReporter) RoomStarted() {
	r.startedAt = time.Now()
	promRoomTotal.Add(1)
	atomic.AddInt32(&atomicRoomTotal, 1)
}

func (r *RoomStatsReporter) RoomEnded() {
	if !r.startedAt.IsZero() {
		promRoomDuration.Observe(float64(time.Now().Sub(r.startedAt)) / float64(time.Second))
	}
	promRoomTotal.Sub(1)
	atomic.AddInt32(&atomicRoomTotal, -1)
}

func (r *RoomStatsReporter) AddParticipant() {
	promParticipantTotal.Add(1)
	atomic.AddInt32(&atomicParticipantTotal, 1)
}

func (r *RoomStatsReporter) SubParticipant() {
	promParticipantTotal.Sub(1)
	atomic.AddInt32(&atomicParticipantTotal, -1)
}

func (r *RoomStatsReporter) AddPublishedTrack(kind string) {
	promTrackPublishedTotal.WithLabelValues(kind).Add(1)
	atomic.AddInt32(&atomicTrackPublishedTotal, 1)
}

func (r *RoomStatsReporter) SubPublishedTrack(kind string) {
	promTrackPublishedTotal.WithLabelValues(kind).Sub(1)
	atomic.AddInt32(&atomicTrackPublishedTotal, -1)
}

func (r *RoomStatsReporter) AddSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Add(1)
	atomic.AddInt32(&atomicTrackSubscribedTotal, 1)
}

func (r *RoomStatsReporter) SubSubscribedTrack(kind string) {
	promTrackSubscribedTotal.WithLabelValues(kind).Sub(1)
	atomic.AddInt32(&atomicTrackSubscribedTotal, -1)
}

func updateCurrentNodeRoomStats(nodeStats *livekit.NodeStats) {
	nodeStats.NumClients = atomic.LoadInt32(&atomicParticipantTotal)
	nodeStats.NumRooms = atomic.LoadInt32(&atomicRoomTotal)
	nodeStats.NumTracksIn = atomic.LoadInt32(&atomicTrackPublishedTotal)
	nodeStats.NumTracksOut = atomic.LoadInt32(&atomicTrackSubscribedTotal)
}
