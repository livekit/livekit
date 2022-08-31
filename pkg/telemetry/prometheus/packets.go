package prometheus

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"
)

type Direction string

const (
	Incoming               Direction = "incoming"
	Outgoing               Direction = "outgoing"
	transmissionInitial              = "initial"
	transmissionRetransmit           = "retransmit"
)

var (
	bytesIn           atomic.Uint64
	bytesOut          atomic.Uint64
	packetsIn         atomic.Uint64
	packetsOut        atomic.Uint64
	nackTotal         atomic.Uint64
	retransmitBytes   atomic.Uint64
	retransmitPackets atomic.Uint64
	participantJoin   atomic.Uint64

	promPacketLabels    = []string{"direction", "transmission"}
	promPacketTotal     *prometheus.CounterVec
	promPacketBytes     *prometheus.CounterVec
	promRTCPLabels      = []string{"direction"}
	promNackTotal       *prometheus.CounterVec
	promPliTotal        *prometheus.CounterVec
	promFirTotal        *prometheus.CounterVec
	promParticipantJoin *prometheus.CounterVec
	promConnections     *prometheus.GaugeVec
)

func initPacketStats(nodeID string) {
	promPacketTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "packet",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, promPacketLabels)
	promPacketBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "packet",
		Name:        "bytes",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, promPacketLabels)
	promNackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "nack",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, promRTCPLabels)
	promPliTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "pli",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, promRTCPLabels)
	promFirTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "fir",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, promRTCPLabels)
	promParticipantJoin = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "participant_join",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, nil)
	promConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "connection",
		Name:        "total",
		ConstLabels: prometheus.Labels{"node_id": nodeID},
	}, []string{"kind"})

	prometheus.MustRegister(promPacketTotal)
	prometheus.MustRegister(promPacketBytes)
	prometheus.MustRegister(promNackTotal)
	prometheus.MustRegister(promPliTotal)
	prometheus.MustRegister(promFirTotal)
	prometheus.MustRegister(promParticipantJoin)
	prometheus.MustRegister(promConnections)
}

func IncrementPackets(direction Direction, count uint64, retransmit bool) {
	promPacketTotal.WithLabelValues(
		string(direction),
		transmissionLabel(retransmit),
	).Add(float64(count))
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
	promPacketBytes.WithLabelValues(
		string(direction),
		transmissionLabel(retransmit),
	).Add(float64(count))
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

func IncrementParticipantJoin(join uint32) {
	if join > 0 {
		promParticipantJoin.WithLabelValues().Add(float64(join))
		participantJoin.Add(uint64(join))
	}
}

func AddConnection(direction Direction) {
	promConnections.WithLabelValues(string(direction)).Add(1)
}

func SubConnection(direction Direction) {
	promConnections.WithLabelValues(string(direction)).Sub(1)
}

func transmissionLabel(retransmit bool) string {
	if !retransmit {
		return transmissionInitial
	} else {
		return transmissionRetransmit
	}
}
