// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"time"

	"github.com/mackerelio/go-osstat/memory"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/hwstats"
)

const (
	livekitNamespace string = "livekit"
)

var (
	initialized atomic.Bool

	MessageCounter            *prometheus.CounterVec
	ServiceOperationCounter   *prometheus.CounterVec
	TwirpRequestStatusCounter *prometheus.CounterVec

	sysPacketsStart              uint32
	sysDroppedPacketsStart       uint32
	promSysPacketGauge           *prometheus.GaugeVec
	promSysDroppedPacketPctGauge prometheus.Gauge

	cpuStats *hwstats.CPUStats
)

func Init(nodeID string, nodeType livekit.NodeType) error {
	if initialized.Swap(true) {
		return nil
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

	ServiceOperationCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "service_operation",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		},
		[]string{"type", "status", "error_type"},
	)

	TwirpRequestStatusCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "twirp_request_status",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		},
		[]string{"service", "method", "status", "code"},
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
	prometheus.MustRegister(ServiceOperationCounter)
	prometheus.MustRegister(TwirpRequestStatusCounter)
	prometheus.MustRegister(promSysPacketGauge)
	prometheus.MustRegister(promSysDroppedPacketPctGauge)

	sysPacketsStart, sysDroppedPacketsStart, _ = getTCStats()

	initPacketStats(nodeID, nodeType)
	initRoomStats(nodeID, nodeType)
	rpc.InitPSRPCStats(prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()})
	initQualityStats(nodeID, nodeType)

	var err error
	cpuStats, err = hwstats.NewCPUStats(nil)
	if err != nil {
		return err
	}

	return nil
}

func GetUpdatedNodeStats(prev *livekit.NodeStats, prevAverage *livekit.NodeStats) (*livekit.NodeStats, bool, error) {
	loadAvg, err := getLoadAvg()
	if err != nil {
		return nil, false, err
	}

	var cpuLoad float64
	cpuIdle := cpuStats.GetCPUIdle()
	if cpuIdle > 0 {
		cpuLoad = 1 - (cpuIdle / cpuStats.NumCPU())
	}

	// On MacOS, get "\"vm_stat\": executable file not found in $PATH" although it is in /usr/bin
	// So, do not error out. Use the information if it is available.
	memTotal := uint64(0)
	memUsed := uint64(0)
	memInfo, _ := memory.Get()
	if memInfo != nil {
		memTotal = memInfo.Total
		memUsed = memInfo.Used
	}

	// do not error out, and use the information if it is available
	sysPackets, sysDroppedPackets, _ := getTCStats()
	promSysPacketGauge.WithLabelValues("out").Set(float64(sysPackets - sysPacketsStart))
	promSysPacketGauge.WithLabelValues("dropped").Set(float64(sysDroppedPackets - sysDroppedPacketsStart))

	bytesInNow := bytesIn.Load()
	bytesOutNow := bytesOut.Load()
	packetsInNow := packetsIn.Load()
	packetsOutNow := packetsOut.Load()
	nackTotalNow := nackTotal.Load()
	retransmitBytesNow := retransmitBytes.Load()
	retransmitPacketsNow := retransmitPackets.Load()
	participantSignalConnectedNow := participantSignalConnected.Load()
	participantRTCInitNow := participantRTCInit.Load()
	participantRTConnectedCNow := participantRTCConnected.Load()
	trackPublishAttemptsNow := trackPublishAttempts.Load()
	trackPublishSuccessNow := trackPublishSuccess.Load()
	trackSubscribeAttemptsNow := trackSubscribeAttempts.Load()
	trackSubscribeSuccessNow := trackSubscribeSuccess.Load()

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
		StartedAt:                        prev.StartedAt,
		UpdatedAt:                        updatedAt,
		NumRooms:                         roomCurrent.Load(),
		NumClients:                       participantCurrent.Load(),
		NumTracksIn:                      trackPublishedCurrent.Load(),
		NumTracksOut:                     trackSubscribedCurrent.Load(),
		NumTrackPublishAttempts:          trackPublishAttemptsNow,
		NumTrackPublishSuccess:           trackPublishSuccessNow,
		NumTrackSubscribeAttempts:        trackSubscribeAttemptsNow,
		NumTrackSubscribeSuccess:         trackSubscribeSuccessNow,
		BytesIn:                          bytesInNow,
		BytesOut:                         bytesOutNow,
		PacketsIn:                        packetsInNow,
		PacketsOut:                       packetsOutNow,
		RetransmitBytesOut:               retransmitBytesNow,
		RetransmitPacketsOut:             retransmitPacketsNow,
		NackTotal:                        nackTotalNow,
		ParticipantSignalConnected:       participantSignalConnectedNow,
		ParticipantRtcInit:               participantRTCInitNow,
		ParticipantRtcConnected:          participantRTConnectedCNow,
		BytesInPerSec:                    prevAverage.BytesInPerSec,
		BytesOutPerSec:                   prevAverage.BytesOutPerSec,
		PacketsInPerSec:                  prevAverage.PacketsInPerSec,
		PacketsOutPerSec:                 prevAverage.PacketsOutPerSec,
		RetransmitBytesOutPerSec:         prevAverage.RetransmitBytesOutPerSec,
		RetransmitPacketsOutPerSec:       prevAverage.RetransmitPacketsOutPerSec,
		NackPerSec:                       prevAverage.NackPerSec,
		ParticipantSignalConnectedPerSec: prevAverage.ParticipantSignalConnectedPerSec,
		ParticipantRtcInitPerSec:         prevAverage.ParticipantRtcInitPerSec,
		ParticipantRtcConnectedPerSec:    prevAverage.ParticipantRtcConnectedPerSec,
		NumCpus:                          uint32(cpuStats.NumCPU()), // this will round down to the nearest integer
		CpuLoad:                          float32(cpuLoad),
		MemoryTotal:                      memTotal,
		MemoryUsed:                       memUsed,
		LoadAvgLast1Min:                  float32(loadAvg.Loadavg1),
		LoadAvgLast5Min:                  float32(loadAvg.Loadavg5),
		LoadAvgLast15Min:                 float32(loadAvg.Loadavg15),
		SysPacketsOut:                    sysPackets,
		SysPacketsDropped:                sysDroppedPackets,
		TrackPublishAttemptsPerSec:       prevAverage.TrackPublishAttemptsPerSec,
		TrackPublishSuccessPerSec:        prevAverage.TrackPublishSuccessPerSec,
		TrackSubscribeAttemptsPerSec:     prevAverage.TrackSubscribeAttemptsPerSec,
		TrackSubscribeSuccessPerSec:      prevAverage.TrackSubscribeSuccessPerSec,
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
		stats.ParticipantSignalConnectedPerSec = perSec(prevAverage.ParticipantSignalConnected, participantSignalConnectedNow, elapsed)
		stats.ParticipantRtcInitPerSec = perSec(prevAverage.ParticipantRtcInit, participantRTCInitNow, elapsed)
		stats.ParticipantRtcConnectedPerSec = perSec(prevAverage.ParticipantRtcConnected, participantRTConnectedCNow, elapsed)
		stats.SysPacketsOutPerSec = perSec(uint64(prevAverage.SysPacketsOut), uint64(sysPackets), elapsed)
		stats.SysPacketsDroppedPerSec = perSec(uint64(prevAverage.SysPacketsDropped), uint64(sysDroppedPackets), elapsed)
		stats.TrackPublishAttemptsPerSec = perSec(uint64(prevAverage.NumTrackPublishAttempts), uint64(trackPublishAttemptsNow), elapsed)
		stats.TrackPublishSuccessPerSec = perSec(uint64(prevAverage.NumTrackPublishSuccess), uint64(trackPublishSuccessNow), elapsed)
		stats.TrackSubscribeAttemptsPerSec = perSec(uint64(prevAverage.NumTrackSubscribeAttempts), uint64(trackSubscribeAttemptsNow), elapsed)
		stats.TrackSubscribeSuccessPerSec = perSec(uint64(prevAverage.NumTrackSubscribeSuccess), uint64(trackSubscribeSuccessNow), elapsed)

		packetTotal := stats.SysPacketsOutPerSec + stats.SysPacketsDroppedPerSec
		if packetTotal == 0 {
			stats.SysPacketsDroppedPctPerSec = 0
		} else {
			stats.SysPacketsDroppedPctPerSec = stats.SysPacketsDroppedPerSec / packetTotal
		}
		promSysDroppedPacketPctGauge.Set(float64(stats.SysPacketsDroppedPctPerSec))
	}

	return stats, computeAverage, nil
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}
