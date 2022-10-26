package prometheus

import (
	"time"

	"github.com/mackerelio/go-osstat/loadavg"
	"github.com/mackerelio/go-osstat/memory"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
)

const (
	livekitNamespace string = "livekit"
)

var (
	initialized atomic.Bool

	MessageCounter          *prometheus.CounterVec
	ServiceOperationCounter *prometheus.CounterVec

	sysPacketsStart              uint32
	sysDroppedPacketsStart       uint32
	promSysPacketGauge           *prometheus.GaugeVec
	promSysDroppedPacketPctGauge prometheus.Gauge
)

func Init(nodeID string) {
	if initialized.Swap(true) {
		return
	}

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

	promSysPacketGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "packet_total",
			ConstLabels: prometheus.Labels{"node_id": nodeID},
			Help:        "System level packet count. Count starts at 0 when service is first started.",
		},
		[]string{"type"},
	)

	promSysDroppedPacketPctGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "dropped_packets",
			ConstLabels: prometheus.Labels{"node_id": nodeID},
			Help:        "System level dropped outgoing packet percentage.",
		},
	)

	prometheus.MustRegister(MessageCounter)
	prometheus.MustRegister(ServiceOperationCounter)
	prometheus.MustRegister(promSysPacketGauge)
	prometheus.MustRegister(promSysDroppedPacketPctGauge)

	sysPacketsStart, sysDroppedPacketsStart, _ = getTCStats()

	initPacketStats(nodeID)
	initRoomStats(nodeID)
}

func getMemoryStats() (memoryLoad float32, err error) {
	memInfo, err := memory.Get()
	if err != nil {
		return
	}

	if memInfo.Total != 0 {
		memoryLoad = float32(memInfo.Used) / float32(memInfo.Total)
	}
	return
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

	memoryLoad, _ := getMemoryStats()
	// On MacOS, get "\"vm_stat\": executable file not found in $PATH" although it is in /usr/bin
	// So, do not error out. Use the information if it is available.

	sysPackets, sysDroppedPackets, err := getTCStats()
	if err != nil {
		return nil, false, err
	}
	promSysPacketGauge.WithLabelValues("out").Set(float64(sysPackets - sysPacketsStart))
	promSysPacketGauge.WithLabelValues("dropped").Set(float64(sysDroppedPackets - sysDroppedPacketsStart))

	bytesInNow := bytesIn.Load()
	bytesOutNow := bytesOut.Load()
	packetsInNow := packetsIn.Load()
	packetsOutNow := packetsOut.Load()
	nackTotalNow := nackTotal.Load()
	retransmitBytesNow := retransmitBytes.Load()
	retransmitPacketsNow := retransmitPackets.Load()
	participantJoinNow := participantJoin.Load()

	updatedAt := time.Now().Unix()
	elapsed := updatedAt - prevAverage.UpdatedAt
	// include sufficient buffer to be sure a stats update had taken place
	computeAverage := elapsed > int64(config.StatsUpdateInterval.Seconds()+2)
	if bytesInNow != prevAverage.BytesIn ||
		bytesOutNow != prevAverage.BytesOut ||
		packetsInNow != prevAverage.PacketsIn ||
		packetsOutNow != prevAverage.PacketsOut ||
		retransmitBytesNow != prevAverage.RetransmitBytesOut ||
		retransmitPacketsNow != prevAverage.RetransmitPacketsOut {
		computeAverage = true
	}

	stats := &livekit.NodeStats{
		StartedAt:                  prev.StartedAt,
		UpdatedAt:                  updatedAt,
		NumRooms:                   roomTotal.Load(),
		NumClients:                 participantTotal.Load(),
		NumTracksIn:                trackPublishedTotal.Load(),
		NumTracksOut:               trackSubscribedTotal.Load(),
		BytesIn:                    bytesInNow,
		BytesOut:                   bytesOutNow,
		PacketsIn:                  packetsInNow,
		PacketsOut:                 packetsOutNow,
		RetransmitBytesOut:         retransmitBytesNow,
		RetransmitPacketsOut:       retransmitPacketsNow,
		NackTotal:                  nackTotalNow,
		ParticipantJoin:            participantJoinNow,
		BytesInPerSec:              prevAverage.BytesInPerSec,
		BytesOutPerSec:             prevAverage.BytesOutPerSec,
		PacketsInPerSec:            prevAverage.PacketsInPerSec,
		PacketsOutPerSec:           prevAverage.PacketsOutPerSec,
		RetransmitBytesOutPerSec:   prevAverage.RetransmitBytesOutPerSec,
		RetransmitPacketsOutPerSec: prevAverage.RetransmitPacketsOutPerSec,
		NackPerSec:                 prevAverage.NackPerSec,
		ParticipantJoinPerSec:      prevAverage.ParticipantJoinPerSec,
		NumCpus:                    numCPUs,
		CpuLoad:                    cpuLoad,
		LoadAvgLast1Min:            float32(loadAvg.Loadavg1),
		LoadAvgLast5Min:            float32(loadAvg.Loadavg5),
		LoadAvgLast15Min:           float32(loadAvg.Loadavg15),
		SysPacketsOut:              sysPackets,
		SysPacketsDropped:          sysDroppedPackets,
		MemoryLoad:                 memoryLoad,
	}

	// update stats
	if computeAverage {
		stats.BytesInPerSec = perSec(prevAverage.BytesIn, bytesInNow, elapsed)
		stats.BytesOutPerSec = perSec(prevAverage.BytesOut, bytesOutNow, elapsed)
		stats.PacketsInPerSec = perSec(prevAverage.PacketsIn, packetsInNow, elapsed)
		stats.PacketsOutPerSec = perSec(prevAverage.PacketsOut, packetsOutNow, elapsed)
		stats.RetransmitBytesOutPerSec = perSec(prevAverage.RetransmitBytesOut, retransmitBytesNow, elapsed)
		stats.RetransmitPacketsOutPerSec = perSec(prevAverage.RetransmitPacketsOut, retransmitPacketsNow, elapsed)
		stats.NackPerSec = perSec(prevAverage.NackTotal, nackTotalNow, elapsed)
		stats.ParticipantJoinPerSec = perSec(prevAverage.ParticipantJoin, participantJoinNow, elapsed)
		stats.SysPacketsOutPerSec = perSec(uint64(prev.SysPacketsOut), uint64(sysPackets), elapsed)
		stats.SysPacketsDroppedPerSec = perSec(uint64(prev.SysPacketsDropped), uint64(sysDroppedPackets), elapsed)

		packetTotal := stats.SysPacketsOutPerSec + stats.SysPacketsDroppedPerSec
		if packetTotal == 0 {
			stats.SysPacketsDroppedPctPerSec = 0
		} else {
			stats.SysPacketsDroppedPctPerSec = float32(stats.SysPacketsDroppedPerSec) / float32(packetTotal)
		}
		promSysDroppedPacketPctGauge.Set(float64(stats.SysPacketsDroppedPctPerSec))
	}

	return stats, computeAverage, nil
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}
