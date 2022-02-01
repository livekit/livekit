package prometheus

import (
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/prometheus/client_golang/prometheus"
)

const livekitNamespace string = "livekit"

var (
	MessageCounter          *prometheus.CounterVec
	ServiceOperationCounter *prometheus.CounterVec
)

func init() {
	nodeID, _ := utils.LocalNodeID()
	MessageCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "messages",
			ConstLabels: prometheus.Labels{"node_id": nodeID},
		},
		[]string{"type", "status"},
	)

	ServiceOperationCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "service_operation",
			ConstLabels: prometheus.Labels{"node_id": nodeID},
		},
		[]string{"type", "status", "error_type"},
	)

	prometheus.MustRegister(MessageCounter)
	prometheus.MustRegister(ServiceOperationCounter)

	initPacketStats(nodeID)
	initRoomStats(nodeID)
}

func GetUpdatedNodeStats(prev *livekit.NodeStats) (*livekit.NodeStats, error) {
	numCPUs, avg1Min, avg5Min, avg15Min, err := getSystemStats()
	if err != nil {
		return nil, err
	}

	updatedAt := time.Now().Unix()
	elapsed := updatedAt - prev.UpdatedAt

	bytesIn := atomic.LoadUint64(&atomicBytesIn)
	bytesOut := atomic.LoadUint64(&atomicBytesOut)
	packetsIn := atomic.LoadUint64(&atomicPacketsIn)
	packetsOut := atomic.LoadUint64(&atomicPacketsOut)
	nackTotal := atomic.LoadUint64(&atomicNackTotal)

	return &livekit.NodeStats{
		StartedAt:        prev.StartedAt,
		UpdatedAt:        updatedAt,
		NumRooms:         atomic.LoadInt32(&atomicRoomTotal),
		NumClients:       atomic.LoadInt32(&atomicParticipantTotal),
		NumTracksIn:      atomic.LoadInt32(&atomicTrackPublishedTotal),
		NumTracksOut:     atomic.LoadInt32(&atomicTrackSubscribedTotal),
		BytesIn:          bytesIn,
		BytesOut:         bytesOut,
		PacketsIn:        packetsIn,
		PacketsOut:       packetsOut,
		NackTotal:        nackTotal,
		BytesInPerSec:    perSec(prev.BytesIn, bytesIn, elapsed),
		BytesOutPerSec:   perSec(prev.BytesOut, bytesOut, elapsed),
		PacketsInPerSec:  perSec(prev.PacketsIn, packetsIn, elapsed),
		PacketsOutPerSec: perSec(prev.PacketsOut, packetsOut, elapsed),
		NackPerSec:       perSec(prev.NackTotal, nackTotal, elapsed),
		NumCpus:          numCPUs,
		LoadAvgLast1Min:  avg1Min,
		LoadAvgLast5Min:  avg5Min,
		LoadAvgLast15Min: avg15Min,
	}, nil
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}
