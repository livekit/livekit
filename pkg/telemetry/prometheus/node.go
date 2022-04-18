package prometheus

import (
	"time"

	"github.com/mackerelio/go-osstat/loadavg"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
)

const (
	livekitNamespace    string = "livekit"
	forceUpdateInterval        = 15
)

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
	loadAvg, err := loadavg.Get()
	if err != nil {
		return nil, err
	}

	cpuLoad, numCPUs, err := getCPUStats()
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

	if bytesInNow == prev.BytesIn &&
		bytesOutNow == prev.BytesOut &&
		packetsInNow == prev.PacketsIn &&
		packetsOutNow == prev.PacketsOut &&
		nackTotalNow == prev.NackTotal &&
		elapsed < forceUpdateInterval {
		return nil, nil
	}

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
		CpuLoad:          cpuLoad,
		LoadAvgLast1Min:  float32(loadAvg.Loadavg1),
		LoadAvgLast5Min:  float32(loadAvg.Loadavg5),
		LoadAvgLast15Min: float32(loadAvg.Loadavg15),
	}, nil
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}
