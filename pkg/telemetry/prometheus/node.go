package prometheus

import (
	"time"

	"github.com/mackerelio/go-osstat/loadavg"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
)

const (
	livekitNamespace string = "livekit"
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

func GetUpdatedNodeStats(prev *livekit.NodeStats, prevAverage *livekit.NodeStats) (*livekit.NodeStats, bool, error) {
	loadAvg, err := loadavg.Get()
	if err != nil {
		return nil, false, err
	}

	cpuLoad, numCPUs, err := getCPUStats()
	if err != nil {
		return nil, false, err
	}

	bytesInNow := bytesIn.Load()
	bytesOutNow := bytesOut.Load()
	packetsInNow := packetsIn.Load()
	packetsOutNow := packetsOut.Load()
	nackTotalNow := nackTotal.Load()

	updatedAt := time.Now().Unix()
	elapsed := updatedAt - prevAverage.UpdatedAt
	// include sufficient buffer to be sure a stats update had taken place
	computeAverage := elapsed > int64(config.StatsUpdateInterval.Seconds()+2)
	if bytesInNow != prevAverage.BytesIn ||
		bytesOutNow != prevAverage.BytesOut ||
		packetsInNow != prevAverage.PacketsIn ||
		packetsOutNow != prevAverage.PacketsOut ||
		nackTotalNow != prevAverage.NackTotal {
		computeAverage = true
	}

	stats := &livekit.NodeStats{
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
		BytesInPerSec:    prevAverage.BytesInPerSec,
		BytesOutPerSec:   prevAverage.BytesOutPerSec,
		PacketsInPerSec:  prevAverage.PacketsInPerSec,
		PacketsOutPerSec: prevAverage.PacketsOutPerSec,
		NackPerSec:       prevAverage.NackPerSec,
		NumCpus:          numCPUs,
		CpuLoad:          cpuLoad,
		LoadAvgLast1Min:  float32(loadAvg.Loadavg1),
		LoadAvgLast5Min:  float32(loadAvg.Loadavg5),
		LoadAvgLast15Min: float32(loadAvg.Loadavg15),
	}

	// update stats
	if computeAverage {
		stats.BytesInPerSec = perSec(prevAverage.BytesIn, bytesInNow, elapsed)
		stats.BytesOutPerSec = perSec(prevAverage.BytesOut, bytesOutNow, elapsed)
		stats.PacketsInPerSec = perSec(prevAverage.PacketsIn, packetsInNow, elapsed)
		stats.PacketsInPerSec = perSec(prevAverage.PacketsOut, packetsOutNow, elapsed)
		stats.NackPerSec = perSec(prevAverage.NackTotal, nackTotalNow, elapsed)
	}

	return stats, computeAverage, nil
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}
