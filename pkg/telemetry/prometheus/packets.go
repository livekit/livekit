package prometheus

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

type Direction string

const (
	Incoming Direction = "incoming"
	Outgoing Direction = "outgoing"
)

var (
	atomicBytesIn    uint64
	atomicBytesOut   uint64
	atomicPacketsIn  uint64
	atomicPacketsOut uint64
	atomicNackTotal  uint64

	promPacketLabels = []string{"direction"}

	promPacketTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "packet",
		Name:      "total",
	}, promPacketLabels)
	promPacketBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "packet",
		Name:      "bytes",
	}, promPacketLabels)
	promNackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "nack",
		Name:      "total",
	}, promPacketLabels)
	promPliTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "pli",
		Name:      "total",
	}, promPacketLabels)
	promFirTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "fir",
		Name:      "total",
	}, promPacketLabels)
)

func initPacketStats() {
	prometheus.MustRegister(promPacketTotal)
	prometheus.MustRegister(promPacketBytes)
	prometheus.MustRegister(promNackTotal)
	prometheus.MustRegister(promPliTotal)
	prometheus.MustRegister(promFirTotal)
}

func IncrementPackets(direction Direction, pktLen uint64) {
	promPacketTotal.WithLabelValues(string(direction)).Add(1)
	promPacketBytes.WithLabelValues(string(direction)).Add(float64(pktLen))
	if direction == Incoming {
		atomic.AddUint64(&atomicPacketsIn, 1)
		atomic.AddUint64(&atomicBytesIn, pktLen)
	} else {
		atomic.AddUint64(&atomicPacketsOut, 1)
		atomic.AddUint64(&atomicBytesOut, pktLen)
	}
}

func IncrementNack(direction Direction) {
	promNackTotal.WithLabelValues(string(direction)).Add(1)
	atomic.AddUint64(&atomicNackTotal, 1)
}

func IncrementPLI(direction Direction) {
	promPliTotal.WithLabelValues(string(direction)).Add(1)
}

func IncrementFIR(direction Direction) {
	promFirTotal.WithLabelValues(string(direction)).Add(1)
}
