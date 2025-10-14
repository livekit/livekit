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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twitchtv/twirp"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/hwstats"
	"github.com/livekit/protocol/webhook"
)

const (
	livekitNamespace string = "livekit"
)

var (
	initialized atomic.Bool

	promMessageCounter            *prometheus.CounterVec
	promServiceOperationCounter   *prometheus.CounterVec
	promTwirpRequestStatusCounter *prometheus.CounterVec

	sysPacketsStart        uint32
	sysDroppedPacketsStart uint32
	promSysPacketGauge     *prometheus.GaugeVec

	cpuStats    *hwstats.CPUStats
	memoryStats *hwstats.MemoryStats
)

func Init(nodeID string, nodeType livekit.NodeType) error {
	if initialized.Swap(true) {
		return nil
	}

	promMessageCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "messages",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		},
		[]string{"type", "status", "direction"},
	)

	promServiceOperationCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   livekitNamespace,
			Subsystem:   "node",
			Name:        "service_operation",
			ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		},
		[]string{"type", "status", "error_type"},
	)

	promTwirpRequestStatusCounter = prometheus.NewCounterVec(
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

	prometheus.MustRegister(promMessageCounter)
	prometheus.MustRegister(promServiceOperationCounter)
	prometheus.MustRegister(promTwirpRequestStatusCounter)
	prometheus.MustRegister(promSysPacketGauge)

	sysPacketsStart, sysDroppedPacketsStart, _ = getTCStats()

	initPacketStats(nodeID, nodeType)
	initRoomStats(nodeID, nodeType)
	rpc.InitPSRPCStats(prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()})
	webhook.InitWebhookStats(prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()})
	initQualityStats(nodeID, nodeType)
	initDataPacketStats(nodeID, nodeType)

	var err error
	cpuStats, err = hwstats.NewCPUStats(nil)
	if err != nil {
		return err
	}

	memoryStats, err = hwstats.NewMemoryStats()
	if err != nil {
		return err
	}

	return nil
}

func GetNodeStats(nodeStartedAt int64, prevStats []*livekit.NodeStats, rateIntervals []time.Duration) (*livekit.NodeStats, error) {
	loadAvg, err := getLoadAvg()
	if err != nil {
		return nil, err
	}

	// On MacOS, get "\"vm_stat\": executable file not found in $PATH" although it is in /usr/bin
	// So, do not error out. Use the information if it is available.
	memUsed, memTotal, _ := memoryStats.GetMemory()

	sysPackets, sysDroppedPackets, _ := getTCStats()
	promSysPacketGauge.WithLabelValues("out").Set(float64(sysPackets - sysPacketsStart))
	promSysPacketGauge.WithLabelValues("dropped").Set(float64(sysDroppedPackets - sysDroppedPacketsStart))

	stats := &livekit.NodeStats{
		StartedAt:                  nodeStartedAt,
		UpdatedAt:                  time.Now().Unix(),
		NumRooms:                   roomCurrent.Load(),
		NumClients:                 participantCurrent.Load(),
		NumTracksIn:                trackPublishedCurrent.Load(),
		NumTracksOut:               trackSubscribedCurrent.Load(),
		NumTrackPublishAttempts:    trackPublishAttempts.Load(),
		NumTrackPublishSuccess:     trackPublishSuccess.Load(),
		NumTrackSubscribeAttempts:  trackSubscribeAttempts.Load(),
		NumTrackSubscribeSuccess:   trackSubscribeSuccess.Load(),
		BytesIn:                    bytesIn.Load(),
		BytesOut:                   bytesOut.Load(),
		PacketsIn:                  packetsIn.Load(),
		PacketsOut:                 packetsOut.Load(),
		RetransmitBytesOut:         retransmitBytes.Load(),
		RetransmitPacketsOut:       retransmitPackets.Load(),
		NackTotal:                  nackTotal.Load(),
		ParticipantSignalConnected: participantSignalConnected.Load(),
		ParticipantRtcInit:         participantRTCInit.Load(),
		ParticipantRtcConnected:    participantRTCConnected.Load(),
		ForwardLatency:             forwardLatency.Load(),
		ForwardJitter:              forwardJitter.Load(),
		NumCpus:                    uint32(cpuStats.NumCPU()), // this will round down to the nearest integer
		CpuLoad:                    float32(cpuStats.GetCPULoad()),
		MemoryTotal:                memTotal,
		MemoryUsed:                 memUsed,
		LoadAvgLast1Min:            float32(loadAvg.Loadavg1),
		LoadAvgLast5Min:            float32(loadAvg.Loadavg5),
		LoadAvgLast15Min:           float32(loadAvg.Loadavg15),
		SysPacketsOut:              sysPackets,
		SysPacketsDropped:          sysDroppedPackets,
	}

	for _, rateInterval := range rateIntervals {
		for idx := len(prevStats) - 1; idx >= 0; idx-- {
			prev := prevStats[idx]
			if prev == nil {
				continue
			}

			if stats.UpdatedAt-prev.UpdatedAt >= int64(rateInterval.Seconds()) {
				if rate := getNodeStatsRate(append(prevStats[idx:], stats)); rate != nil {
					stats.Rates = append(stats.Rates, rate)
				}
				break
			}
		}
	}

	return stats, nil
}

func getNodeStatsRate(statsHistory []*livekit.NodeStats) *livekit.NodeStatsRate {
	if len(statsHistory) == 0 {
		return nil
	}

	elapsed := statsHistory[len(statsHistory)-1].UpdatedAt - statsHistory[0].UpdatedAt
	if elapsed <= 0 {
		return nil
	}

	// time weighted averages
	var cpuLoad, memoryUsed, memoryTotal, memoryLoad float32
	for idx := len(statsHistory) - 1; idx > 0; idx-- {
		stats := statsHistory[idx]
		prevStats := statsHistory[idx-1]
		if stats == nil || prevStats == nil {
			continue
		}

		spanElapsed := stats.UpdatedAt - prevStats.UpdatedAt
		if spanElapsed <= 0 {
			continue
		}

		cpuLoad += stats.CpuLoad * float32(spanElapsed)
		memoryUsed += float32(stats.MemoryUsed) * float32(spanElapsed)
		memoryTotal += float32(stats.MemoryTotal) * float32(spanElapsed)
		if stats.MemoryTotal > 0 {
			memoryLoad += float32(stats.MemoryUsed) / float32(stats.MemoryTotal) * float32(spanElapsed)
		}
	}

	earlier := statsHistory[0]
	later := statsHistory[len(statsHistory)-1]
	rate := &livekit.NodeStatsRate{
		StartedAt:                  earlier.UpdatedAt,
		EndedAt:                    later.UpdatedAt,
		Duration:                   elapsed,
		BytesIn:                    perSec(earlier.BytesIn, later.BytesIn, elapsed),
		BytesOut:                   perSec(earlier.BytesOut, later.BytesOut, elapsed),
		PacketsIn:                  perSec(earlier.PacketsIn, later.PacketsIn, elapsed),
		PacketsOut:                 perSec(earlier.PacketsOut, later.PacketsOut, elapsed),
		RetransmitBytesOut:         perSec(earlier.RetransmitBytesOut, later.RetransmitBytesOut, elapsed),
		RetransmitPacketsOut:       perSec(earlier.RetransmitPacketsOut, later.RetransmitPacketsOut, elapsed),
		NackTotal:                  perSec(earlier.NackTotal, later.NackTotal, elapsed),
		ParticipantSignalConnected: perSec(earlier.ParticipantSignalConnected, later.ParticipantSignalConnected, elapsed),
		ParticipantRtcInit:         perSec(earlier.ParticipantRtcInit, later.ParticipantRtcInit, elapsed),
		ParticipantRtcConnected:    perSec(earlier.ParticipantRtcConnected, later.ParticipantRtcConnected, elapsed),
		SysPacketsOut:              perSec(uint64(earlier.SysPacketsOut), uint64(later.SysPacketsOut), elapsed),
		SysPacketsDropped:          perSec(uint64(earlier.SysPacketsDropped), uint64(later.SysPacketsDropped), elapsed),
		TrackPublishAttempts:       perSec(uint64(earlier.NumTrackPublishAttempts), uint64(later.NumTrackPublishAttempts), elapsed),
		TrackPublishSuccess:        perSec(uint64(earlier.NumTrackPublishSuccess), uint64(later.NumTrackPublishSuccess), elapsed),
		TrackSubscribeAttempts:     perSec(uint64(earlier.NumTrackSubscribeAttempts), uint64(later.NumTrackSubscribeAttempts), elapsed),
		TrackSubscribeSuccess:      perSec(uint64(earlier.NumTrackSubscribeSuccess), uint64(later.NumTrackSubscribeSuccess), elapsed),
		CpuLoad:                    cpuLoad / float32(elapsed),
		MemoryLoad:                 memoryLoad / float32(elapsed),
		MemoryUsed:                 memoryUsed / float32(elapsed),
		MemoryTotal:                memoryTotal / float32(elapsed),
	}
	return rate
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}

func RecordSignalRequestSuccess() {
	promMessageCounter.WithLabelValues("signal", "success", "request").Add(1)
}

func RecordSignalRequestFailure() {
	promMessageCounter.WithLabelValues("signal", "failure", "request").Add(1)
}

func RecordSignalResponseSuccess() {
	promMessageCounter.WithLabelValues("signal", "success", "response").Add(1)
}

func RecordSignalResponseFailure() {
	promMessageCounter.WithLabelValues("signal", "failure", "response").Add(1)
}

func RecordServiceOperationSuccess(op string) {
	promServiceOperationCounter.WithLabelValues(op, "success", "").Add(1)
}

func RecordServiceOperationError(op string, error string) {
	promServiceOperationCounter.WithLabelValues(op, "error", error).Add(1)
}

func RecordTwirpRequestStatus(service string, method string, statusFamily string, code twirp.ErrorCode) {
	promTwirpRequestStatusCounter.WithLabelValues(service, method, statusFamily, string(code)).Add(1)
}
