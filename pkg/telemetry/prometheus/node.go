package prometheus

import (
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

	bytesInNow := bytesIn.Load()
	bytesOutNow := bytesOut.Load()
	packetsInNow := packetsIn.Load()
	packetsOutNow := packetsOut.Load()
	nackTotalNow := nackTotal.Load()

	return &livekit.NodeStats{
		StartedAt:        prev.StartedAt,
		UpdatedAt:        updatedAt,
		NumRooms:         roomTotal.Load(),
		NumClients:       participantTotal.Load(),
		NumTracksIn:      trackPublishedTotal.Load(),
		NumTracksOut:     trackSubscribedTotal.Load(),
		BytesIn:          bytesInNow,
		BytesOut:         bytesOutNow,
		PacketsIn:        packetsInNow,
		PacketsOut:       packetsOutNow,
		NackTotal:        nackTotalNow,
		BytesInPerSec:    perSec(prev.BytesIn, bytesInNow, elapsed),
		BytesOutPerSec:   perSec(prev.BytesOut, bytesOutNow, elapsed),
		PacketsInPerSec:  perSec(prev.PacketsIn, packetsInNow, elapsed),
		PacketsOutPerSec: perSec(prev.PacketsOut, packetsOutNow, elapsed),
		NackPerSec:       perSec(prev.NackTotal, nackTotalNow, elapsed),
		NumCpus:          numCPUs,
		LoadAvgLast1Min:  avg1Min,
		LoadAvgLast5Min:  avg5Min,
		LoadAvgLast15Min: avg15Min,
	}, nil
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}
