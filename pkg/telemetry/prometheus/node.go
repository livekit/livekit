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

	MessageCounter *prometheus.CounterVec

	connectionFailures           atomic.Uint64
	connectionSuccess            atomic.Uint64
	signalFailures               atomic.Uint64
	signalSuccess                atomic.Uint64
	serviceOperationCounter      *prometheus.CounterVec
	sysPacketsStart              uint32
	sysDroppedPacketsStart       uint32
	promSysPacketGauge           *prometheus.GaugeVec
	promSysDroppedPacketPctGauge prometheus.Gauge
)

func Init(nodeID string, nodeType livekit.NodeType) {
	if initialized.Swap(true) {
		return
	}

	MessageCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "messages",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		},
		[]string{"type", "status"},
	)

	serviceOperationCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "service_operation",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		},
		[]string{"type", "status", "error_type"},
	)

	promSysPacketGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "packet_total",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
			Help:        "System level packet count. Count starts at 0 when service is first started.",
		},
		[]string{"type"},
	)

	promSysDroppedPacketPctGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "dropped_packets",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
			Help:        "System level dropped outgoing packet percentage.",
		},
	)

	prometheus.MustRegister(MessageCounter)
	prometheus.MustRegister(serviceOperationCounter)
	prometheus.MustRegister(promSysPacketGauge)
	prometheus.MustRegister(promSysDroppedPacketPctGauge)

	sysPacketsStart, sysDroppedPacketsStart, _ = getTCStats()

	initPacketStats(nodeID, nodeType)
	initRoomStats(nodeID, nodeType)
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

func AddServiceOperation(typeStr string, status string, errType string) {
	switch typeStr {
	case "ice_connection", "peer_connection", "offer", "answer":
		if errType == "" {
			connectionSuccess.Inc()
		} else {
			connectionFailures.Inc()
		}
	case "participant_join":
		// errors can: room_closed, already_joined, max_exceeded, send_reponse
		if errType == "" {
			IncrementParticipantJoin(1)
		} else {
			IncrementParticipantJoinFailed(1)
		}

	case "signal_ws":
		if errType == "" {
			signalSuccess.Inc()
		} else {
			if errType == "initial_response" || errType == "reject" {
				// ignore errors based on bad client behavior
				break
			}
			signalFailures.Inc()
		}
	}

	serviceOperationCounter.WithLabelValues(typeStr, status, errType).Add(1)
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
	connectionSuccessNow := connectionSuccess.Load()
	signalSuccessNow := signalSuccess.Load()
	trackPublishedTotalNow := trackPublishedTotal.Load()
	trackSubscribedTotalNow := trackSubscribedTotal.Load()

	// failures
	participantJoinFailedNow := participantJoinFailed.Load()
	connectionFailuresNow := connectionFailures.Load()
	signalFailuresNow := signalFailures.Load()
	trackPublishedFailureTotalNow := trackPublishedFailureTotal.Load()
	trackSubscribedFailureTotalNow := trackSubscribedFailureTotal.Load()

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
		NumTracksIn:                trackPublishedTotalNow,
		NumTracksOut:               trackSubscribedTotalNow,
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
		ConnectionSuccess:          connectionSuccessNow,
		ConnectionFailures:         connectionFailuresNow,
		SignalSuccess:              signalSuccessNow,
		SignalFailures:             signalFailuresNow,
		TrackPublishedFailures:     uint64(trackPublishedFailureTotalNow),
		TrackSubscribedFailures:    uint64(trackSubscribedFailureTotalNow),
		ParticipantJoinFailures:    participantJoinFailedNow,
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

		stats.ConnectionSuccessPerSec = perSec(prevAverage.ConnectionSuccess, connectionSuccessNow, elapsed)
		stats.ConnectionFailuresPerSec = perSec(prevAverage.ConnectionFailures, connectionFailuresNow, elapsed)
		stats.SignalSuccessPerSec = perSec(prevAverage.SignalSuccess, signalSuccessNow, elapsed)
		stats.SignalFailuresPerSec = perSec(prevAverage.SignalFailures, signalFailuresNow, elapsed)
		stats.TrackPublishedPerSec = perSec(uint64(prevAverage.NumTracksIn), uint64(trackPublishedTotalNow), elapsed)
		stats.TrackPublishedFailurePerSec = perSec(prevAverage.TrackPublishedFailures, uint64(trackPublishedFailureTotalNow), elapsed)
		stats.TrackSubscribedPerSec = perSec(uint64(prevAverage.NumTracksOut), uint64(trackSubscribedTotalNow), elapsed)
		stats.TrackSubscribedFailurePerSec = perSec(prevAverage.TrackSubscribedFailures, uint64(trackSubscribedFailureTotalNow), elapsed)
		stats.ParticipantJoinFailurePerSec = perSec(prevAverage.ParticipantJoinFailures, participantJoinFailedNow, elapsed)
	}

	return stats, computeAverage, nil
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}
