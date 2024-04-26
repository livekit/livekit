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
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
)

type Direction string

const (
	Incoming               Direction = "incoming"
	Outgoing               Direction = "outgoing"
	transmissionInitial              = "initial"
	transmissionRetransmit           = "retransmit"
)

var (
	bytesIn                    atomic.Uint64
	bytesOut                   atomic.Uint64
	packetsIn                  atomic.Uint64
	packetsOut                 atomic.Uint64
	nackTotal                  atomic.Uint64
	retransmitBytes            atomic.Uint64
	retransmitPackets          atomic.Uint64
	participantSignalConnected atomic.Uint64
	participantRTCConnected    atomic.Uint64
	participantRTCInit         atomic.Uint64

	promPacketLabels    = []string{"direction", "transmission"}
	promPacketTotal     *prometheus.CounterVec
	promPacketBytes     *prometheus.CounterVec
	promRTCPLabels      = []string{"direction"}
	promStreamLabels    = []string{"direction", "source", "type"}
	promNackTotal       *prometheus.CounterVec
	promPliTotal        *prometheus.CounterVec
	promFirTotal        *prometheus.CounterVec
	promPacketLossTotal *prometheus.CounterVec
	promPacketLoss      *prometheus.HistogramVec
	promJitter          *prometheus.HistogramVec
	promRTT             *prometheus.HistogramVec
	promParticipantJoin *prometheus.CounterVec
	promConnections     *prometheus.GaugeVec

	promPacketTotalIncomingInitial    prometheus.Counter
	promPacketTotalIncomingRetransmit prometheus.Counter
	promPacketTotalOutgoingInitial    prometheus.Counter
	promPacketTotalOutgoingRetransmit prometheus.Counter
	promPacketBytesIncomingInitial    prometheus.Counter
	promPacketBytesIncomingRetransmit prometheus.Counter
	promPacketBytesOutgoingInitial    prometheus.Counter
	promPacketBytesOutgoingRetransmit prometheus.Counter
)

func initPacketStats(nodeID string, nodeType livekit.NodeType) {
	promPacketTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "packet",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, promPacketLabels)
	promPacketBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "packet",
		Name:        "bytes",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, promPacketLabels)
	promNackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "nack",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, promRTCPLabels)
	promPliTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "pli",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, promRTCPLabels)
	promFirTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "fir",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, promRTCPLabels)
	promPacketLossTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "packet_loss",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, promStreamLabels)
	promPacketLoss = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "packet_loss",
		Name:        "percent",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     []float64{0.0, 0.1, 0.3, 0.5, 0.7, 1, 5, 10, 40, 100},
	}, promStreamLabels)
	promJitter = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "jitter",
		Name:        "us",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},

		// 1ms, 10ms, 30ms, 50ms, 70ms, 100ms, 300ms, 600ms, 1s
		Buckets: []float64{1000, 10000, 30000, 50000, 70000, 100000, 300000, 600000, 1000000},
	}, promStreamLabels)
	promRTT = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "rtt",
		Name:        "ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     []float64{50, 100, 150, 200, 250, 500, 750, 1000, 5000, 10000},
	}, promStreamLabels)
	promParticipantJoin = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "participant_join",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"state"})
	promConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "connection",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"kind"})

	prometheus.MustRegister(promPacketTotal)
	prometheus.MustRegister(promPacketBytes)
	prometheus.MustRegister(promNackTotal)
	prometheus.MustRegister(promPliTotal)
	prometheus.MustRegister(promFirTotal)
	prometheus.MustRegister(promPacketLossTotal)
	prometheus.MustRegister(promPacketLoss)
	prometheus.MustRegister(promJitter)
	prometheus.MustRegister(promRTT)
	prometheus.MustRegister(promParticipantJoin)
	prometheus.MustRegister(promConnections)

	promPacketTotalIncomingInitial = promPacketTotal.WithLabelValues(string(Incoming), transmissionInitial)
	promPacketTotalIncomingRetransmit = promPacketTotal.WithLabelValues(string(Incoming), transmissionRetransmit)
	promPacketTotalOutgoingInitial = promPacketTotal.WithLabelValues(string(Outgoing), transmissionInitial)
	promPacketTotalOutgoingRetransmit = promPacketTotal.WithLabelValues(string(Outgoing), transmissionRetransmit)
	promPacketBytesIncomingInitial = promPacketBytes.WithLabelValues(string(Incoming), transmissionInitial)
	promPacketBytesIncomingRetransmit = promPacketBytes.WithLabelValues(string(Incoming), transmissionRetransmit)
	promPacketBytesOutgoingInitial = promPacketBytes.WithLabelValues(string(Outgoing), transmissionInitial)
	promPacketBytesOutgoingRetransmit = promPacketBytes.WithLabelValues(string(Outgoing), transmissionRetransmit)
}

func IncrementPackets(direction Direction, count uint64, retransmit bool) {
	if direction == Incoming {
		if retransmit {
			promPacketTotalIncomingRetransmit.Add(float64(count))
		} else {
			promPacketTotalIncomingInitial.Add(float64(count))
		}
	} else {
		if retransmit {
			promPacketTotalOutgoingRetransmit.Add(float64(count))
		} else {
			promPacketTotalOutgoingInitial.Add(float64(count))
		}
	}

	if direction == Incoming {
		packetsIn.Add(count)
	} else {
		packetsOut.Add(count)
		if retransmit {
			retransmitPackets.Add(count)
		}
	}
}

func IncrementBytes(direction Direction, count uint64, retransmit bool) {
	if direction == Incoming {
		if retransmit {
			promPacketBytesIncomingRetransmit.Add(float64(count))
		} else {
			promPacketBytesIncomingInitial.Add(float64(count))
		}
	} else {
		if retransmit {
			promPacketBytesOutgoingRetransmit.Add(float64(count))
		} else {
			promPacketBytesOutgoingInitial.Add(float64(count))
		}
	}

	if direction == Incoming {
		bytesIn.Add(count)
	} else {
		bytesOut.Add(count)
		if retransmit {
			retransmitBytes.Add(count)
		}
	}
}

func IncrementRTCP(direction Direction, nack, pli, fir uint32) {
	if nack > 0 {
		promNackTotal.WithLabelValues(string(direction)).Add(float64(nack))
		nackTotal.Add(uint64(nack))
	}
	if pli > 0 {
		promPliTotal.WithLabelValues(string(direction)).Add(float64(pli))
	}
	if fir > 0 {
		promFirTotal.WithLabelValues(string(direction)).Add(float64(fir))
	}
}

func RecordPacketLoss(direction Direction, trackSource livekit.TrackSource, trackType livekit.TrackType, lost, total uint32) {
	if total > 0 {
		promPacketLoss.WithLabelValues(string(direction), trackSource.String(), trackType.String()).Observe(float64(lost) / float64(total) * 100)
	}
	if lost > 0 {
		promPacketLossTotal.WithLabelValues(string(direction), trackSource.String(), trackType.String()).Add(float64(lost))
	}
}

func RecordJitter(direction Direction, trackSource livekit.TrackSource, trackType livekit.TrackType, jitter uint32) {
	if jitter > 0 {
		promJitter.WithLabelValues(string(direction), trackSource.String(), trackType.String()).Observe(float64(jitter))
	}
}

func RecordRTT(direction Direction, trackSource livekit.TrackSource, trackType livekit.TrackType, rtt uint32) {
	if rtt > 0 {
		promRTT.WithLabelValues(string(direction), trackSource.String(), trackType.String()).Observe(float64(rtt))
	}
}

func IncrementParticipantJoin(join uint32) {
	if join > 0 {
		participantSignalConnected.Add(uint64(join))
		promParticipantJoin.WithLabelValues("signal_connected").Add(float64(join))
	}
}

func IncrementParticipantJoinFail(join uint32) {
	if join > 0 {
		promParticipantJoin.WithLabelValues("signal_failed").Add(float64(join))
	}
}

func IncrementParticipantRtcInit(join uint32) {
	if join > 0 {
		participantRTCInit.Add(uint64(join))
		promParticipantJoin.WithLabelValues("rtc_init").Add(float64(join))
	}
}

func IncrementParticipantRtcConnected(join uint32) {
	if join > 0 {
		participantRTCConnected.Add(uint64(join))
		promParticipantJoin.WithLabelValues("rtc_connected").Add(float64(join))
	}
}

func AddConnection(direction Direction) {
	promConnections.WithLabelValues(string(direction)).Add(1)
}

func SubConnection(direction Direction) {
	promConnections.WithLabelValues(string(direction)).Sub(1)
}
