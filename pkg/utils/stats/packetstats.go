package stats

import (
	"sync/atomic"

	livekit "github.com/livekit/protocol/proto"
	"github.com/pion/rtcp"
	"github.com/prometheus/client_golang/prometheus"
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
	if s.direction == "incoming" {
		atomic.AddUint64(&atomicBytesIn, bytes)
	} else {
		atomic.AddUint64(&atomicBytesOut, bytes)
	}
}

func (s *PacketStats) IncrementPackets(count uint64) {
	promPacketTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.PacketTotal, count)
	if s.direction == "incoming" {
		atomic.AddUint64(&atomicPacketsIn, count)
	} else {
		atomic.AddUint64(&atomicPacketsOut, count)
	}
}

func (s *PacketStats) IncrementNack(count uint64) {
	promNackTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.NackTotal, count)
	atomic.AddUint64(&atomicNackTotal, count)
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

func updateCurrentNodePacketStats(nodeStats *livekit.NodeStats, secondsSinceLastUpdate int64) {
	if secondsSinceLastUpdate == 0 {
		return
	}

	bytesInPrevious := nodeStats.BytesIn
	bytesOutPrevious := nodeStats.BytesOut
	packetsInPrevious := nodeStats.PacketsIn
	packetsOutPrevious := nodeStats.PacketsOut
	nackTotalPrevious := nodeStats.NackTotal

	nodeStats.BytesIn = atomic.LoadUint64(&atomicBytesIn)
	nodeStats.BytesOut = atomic.LoadUint64(&atomicBytesOut)
	nodeStats.PacketsIn = atomic.LoadUint64(&atomicPacketsIn)
	nodeStats.PacketsOut = atomic.LoadUint64(&atomicPacketsOut)
	nodeStats.NackTotal = atomic.LoadUint64(&atomicNackTotal)

	nodeStats.BytesInPerSec = perSec(bytesInPrevious, nodeStats.BytesIn, secondsSinceLastUpdate)
	nodeStats.BytesOutPerSec = perSec(bytesOutPrevious, nodeStats.BytesOut, secondsSinceLastUpdate)
	nodeStats.PacketsInPerSec = perSec(packetsInPrevious, nodeStats.PacketsIn, secondsSinceLastUpdate)
	nodeStats.PacketsOutPerSec = perSec(packetsOutPrevious, nodeStats.PacketsOut, secondsSinceLastUpdate)
	nodeStats.NackPerSec = perSec(nackTotalPrevious, nodeStats.NackTotal, secondsSinceLastUpdate)
}

func perSec(prev, curr uint64, secs int64) float32 {
	return float32(curr-prev) / float32(secs)
}
