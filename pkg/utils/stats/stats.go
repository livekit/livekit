package stats

import (
	"io"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/transport/packetio"
	"github.com/prometheus/client_golang/prometheus"
)

const livekitNamespace = "livekit"

var (
	promLabels  = []string{"direction"}
	packetTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "packet",
		Name:      "total",
	}, promLabels)
	packetBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "packet",
		Name:      "bytes",
	}, promLabels)
	nackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "nack",
		Name:      "total",
	}, promLabels)
	pliTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "pli",
		Name:      "total",
	}, promLabels)
	firTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: livekitNamespace,
		Subsystem: "fir",
		Name:      "total",
	}, promLabels)
	roomTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: livekitNamespace,
		Subsystem: "room",
		Name:      "total",
	})
	roomDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: livekitNamespace,
		Subsystem: "room",
		Name:      "duration_seconds",
		Buckets: []float64{
			5, 10, 60, 5 * 60, 10 * 60, 30 * 60, 60 * 60, 2 * 60 * 60, 5 * 60 * 60, 10 * 60 * 60,
		},
	})
	participantTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: livekitNamespace,
		Subsystem: "participant",
		Name:      "total",
	})
	trackPublishedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: livekitNamespace,
		Subsystem: "track",
		Name:      "published_total",
	}, []string{"kind"})
	trackSubscribedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: livekitNamespace,
		Subsystem: "track",
		Name:      "subscribed_total",
	}, []string{"kind"})
)

func init() {
	prometheus.MustRegister(packetTotal)
	prometheus.MustRegister(packetBytes)
	prometheus.MustRegister(nackTotal)
	prometheus.MustRegister(pliTotal)
	prometheus.MustRegister(firTotal)
	prometheus.MustRegister(roomTotal)
	prometheus.MustRegister(roomDuration)
	prometheus.MustRegister(participantTotal)
	prometheus.MustRegister(trackPublishedTotal)
	prometheus.MustRegister(trackSubscribedTotal)
}

// RoomStatsReporter is created for each room
type RoomStatsReporter struct {
	roomName  string
	startedAt time.Time
	Incoming  *PacketStats
	Outgoing  *PacketStats
}

func NewRoomStatsReporter(roomName string) *RoomStatsReporter {
	return &RoomStatsReporter{
		roomName: roomName,
		Incoming: newPacketStats(roomName, "incoming"),
		Outgoing: newPacketStats(roomName, "outgoing"),
	}
}

func (r *RoomStatsReporter) RoomStarted() {
	r.startedAt = time.Now()
	roomTotal.Add(1)
}

func (r *RoomStatsReporter) RoomEnded() {
	if !r.startedAt.IsZero() {
		roomDuration.Observe(float64(time.Now().Sub(r.startedAt)) / float64(time.Second))
	}
	roomTotal.Sub(1)
}

func (r *RoomStatsReporter) AddParticipant() {
	participantTotal.Add(1)
}

func (r *RoomStatsReporter) SubParticipant() {
	participantTotal.Sub(1)
}

func (r *RoomStatsReporter) AddPublishedTrack(kind string) {
	trackPublishedTotal.WithLabelValues(kind).Add(1)
}

func (r *RoomStatsReporter) SubPublishedTrack(kind string) {
	trackPublishedTotal.WithLabelValues(kind).Sub(1)
}

func (r *RoomStatsReporter) AddSubscribedTrack(kind string) {
	trackSubscribedTotal.WithLabelValues(kind).Add(1)
}

func (r *RoomStatsReporter) SubSubscribedTrack(kind string) {
	trackSubscribedTotal.WithLabelValues(kind).Sub(1)
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
	packetBytes.WithLabelValues(s.direction).Add(float64(bytes))
	atomic.AddUint64(&s.PacketBytes, bytes)
}

func (s *PacketStats) IncrementPackets(count uint64) {
	packetTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.PacketTotal, count)
}

func (s *PacketStats) IncrementNack(count uint64) {
	nackTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.NackTotal, count)
}

func (s *PacketStats) IncrementPLI(count uint64) {
	pliTotal.WithLabelValues(s.direction).Add(float64(count))
	atomic.AddUint64(&s.PLITotal, count)
}

func (s *PacketStats) IncrementFIR(count uint64) {
	firTotal.WithLabelValues(s.direction).Add(float64(count))
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

// StatsBufferWrapper wraps a buffer factory so we could get information on
// incoming packets
type StatsBufferWrapper struct {
	CreateBufferFunc func(packetType packetio.BufferPacketType, ssrc uint32) io.ReadWriteCloser
	Stats            *PacketStats
}

func (w *StatsBufferWrapper) CreateBuffer(packetType packetio.BufferPacketType, ssrc uint32) io.ReadWriteCloser {
	writer := w.CreateBufferFunc(packetType, ssrc)
	if packetType == packetio.RTPBufferPacket {
		// wrap this in a counter class
		return &rtpReporterWriter{
			ReadWriteCloser: writer,
			stats:           w.Stats,
		}
	}
	return writer
}

type rtpReporterWriter struct {
	io.ReadWriteCloser
	stats *PacketStats
}

func (w *rtpReporterWriter) Write(p []byte) (n int, err error) {
	w.stats.IncrementPackets(1)
	w.stats.IncrementBytes(uint64(len(p)))
	return w.ReadWriteCloser.Write(p)
}

// StatsInterceptor is created for each participant to keep of track of outgoing stats
// it adheres to Pion interceptor interface
type StatsInterceptor struct {
	interceptor.NoOp
	reporter *RoomStatsReporter
}

func NewStatsInterceptor(reporter *RoomStatsReporter) *StatsInterceptor {
	return &StatsInterceptor{
		reporter: reporter,
	}
}

// BindRTCPWriter lets you modify any outgoing RTCP packets. It is called once per PeerConnection. The returned method
// will be called once per packet batch.
func (s *StatsInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		s.reporter.Outgoing.HandleRTCP(pkts)
		return writer.Write(pkts, attributes)
	})
}

// BindLocalStream lets you modify any outgoing RTP packets. It is called once for per LocalStream. The returned method
// will be called once per rtp packet.
func (s *StatsInterceptor) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		s.reporter.Outgoing.IncrementPackets(1)
		s.reporter.Outgoing.IncrementBytes(uint64(len(payload)))
		return writer.Write(header, payload, attributes)
	})
}
