package stats

import (
	"sync/atomic"

	"github.com/pion/rtcp"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	promPacketTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "packet",
		Name:      "total",
	}, promLabels)
	promPacketBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "packet",
		Name:      "bytes",
	}, promLabels)
	promNackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "nack",
		Name:      "total",
	}, promLabels)
	promPliTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "pli",
		Name:      "total",
	}, promLabels)
	promFirTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "fir",
		Name:      "total",
	}, promLabels)
)

func initPacketStats() {
	prometheus.MustRegister(promPacketTotal)
	prometheus.MustRegister(promPacketBytes)
	prometheus.MustRegister(promNackTotal)
	prometheus.MustRegister(promPliTotal)
	prometheus.MustRegister(promFirTotal)
}

type PacketStats struct {
	roomName  string
	direction string // incoming or outgoing

	PacketBytes uint64 `json:"packetBytes"`
	PacketTotal uint64 `json:"packetTotal"`
	NackTotal   uint64 `json:"nackTotal"`
	PLITotal    uint64 `json:"pliTotal"`
	FIRTotal    uint64 `json:"firTotal"`
}

func newPacketStats(room, direction string) *PacketStats {
	return &PacketStats{
		roomName:  room,
		direction: direction,
	}
}

func (s *PacketStats) IncrementBytes(bytes uint64) {
	promPacketBytes.WithLabelValues(s.direction).Add(float64(bytes))
	atomic.AddUint64(&s.PacketBytes, bytes)
}

func (s *PacketStats) IncrementPackets(count uint64) {
	promPacketTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.PacketTotal, count)
}

func (s *PacketStats) IncrementNack(count uint64) {
	promNackTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.NackTotal, count)
}

func (s *PacketStats) IncrementPLI(count uint64) {
	promPliTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.PLITotal, count)
}

func (s *PacketStats) IncrementFIR(count uint64) {
	promFirTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.FIRTotal, count)
}

func (s *PacketStats) HandleRTCP(pkts []rtcp.Packet) {
	for _, rtcpPacket := range pkts {
		switch rtcpPacket.(type) {
		case *rtcp.TransportLayerNack:
			s.IncrementNack(1)
		case *rtcp.PictureLossIndication:
			s.IncrementPLI(1)
		case *rtcp.FullIntraRequest:
			s.IncrementFIR(1)
		}
	}
}

func (s PacketStats) Copy() *PacketStats {
	return &PacketStats{
		roomName:    s.roomName,
		direction:   s.direction,
		PacketBytes: atomic.LoadUint64(&s.PacketBytes),
		PacketTotal: atomic.LoadUint64(&s.PacketTotal),
		NackTotal:   atomic.LoadUint64(&s.NackTotal),
		PLITotal:    atomic.LoadUint64(&s.PLITotal),
		FIRTotal:    atomic.LoadUint64(&s.FIRTotal),
	}
}
