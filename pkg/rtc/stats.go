package rtc

import (
	"io"
	"sync/atomic"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/transport/packetio"
)

type PacketStats struct {
	roomName string
	identity string
	kind     string // incoming or outgoing

	PacketCount uint64 `json:"packetCount"`
	NackCount   uint64 `json:"nackCount"`
	PLICount    uint64 `json:"pliCount"`
	FIRCount    uint64 `json:"firCount"`
}

func newPacketStats(room, identity, kind string) *PacketStats {
	return &PacketStats{
		roomName: room,
		identity: identity,
		kind:     kind,
	}
}

func (s *PacketStats) IncrementPackets(count uint64) {
	atomic.AddUint64(&s.PacketCount, count)
}

func (s *PacketStats) IncrementNack(count uint64) {
	atomic.AddUint64(&s.NackCount, count)
}

func (s *PacketStats) IncrementPLI(count uint64) {
	atomic.AddUint64(&s.PLICount, count)
}

func (s *PacketStats) IncrementFIR(count uint64) {
	atomic.AddUint64(&s.FIRCount, count)
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

func (s *PacketStats) addFrom(o *PacketStats) {
	s.PacketCount += atomic.LoadUint64(&o.PacketCount)
	s.NackCount += atomic.LoadUint64(&o.NackCount)
	s.PLICount += atomic.LoadUint64(&o.PLICount)
	s.FIRCount += atomic.LoadUint64(&o.FIRCount)
}

type StatsReporter struct {
	incoming *PacketStats
	outgoing *PacketStats
}

func NewStatsReporter(roomName, identity string) *StatsReporter {
	return &StatsReporter{
		incoming: newPacketStats(roomName, identity, "incoming"),
		outgoing: newPacketStats(roomName, identity, "outgoing"),
	}
}

// StatsBufferWrapper wraps a buffer factory so we could get information on
// incoming packets
type StatsBufferWrapper struct {
	createBufferFunc func(packetType packetio.BufferPacketType, ssrc uint32) io.ReadWriteCloser
	stats            *PacketStats
}

func (w *StatsBufferWrapper) CreateBuffer(packetType packetio.BufferPacketType, ssrc uint32) io.ReadWriteCloser {
	writer := w.createBufferFunc(packetType, ssrc)
	if packetType == packetio.RTPBufferPacket {
		// wrap this in a counter class
		return &rtpReporterWriter{
			ReadWriteCloser: writer,
			stats:           w.stats,
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
	return w.ReadWriteCloser.Write(p)
}

// StatsInterceptor is created for each participant to keep of track of outgoing stats
// it adheres to Pion interceptor interface
type StatsInterceptor struct {
	interceptor.NoOp
	reporter *StatsReporter
}

func NewStatsInterceptor(reporter *StatsReporter) *StatsInterceptor {
	return &StatsInterceptor{
		reporter: reporter,
	}
}

// BindRTCPWriter lets you modify any outgoing RTCP packets. It is called once per PeerConnection. The returned method
// will be called once per packet batch.
func (s *StatsInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		s.reporter.outgoing.HandleRTCP(pkts)
		return writer.Write(pkts, attributes)
	})
}

// BindLocalStream lets you modify any outgoing RTP packets. It is called once for per LocalStream. The returned method
// will be called once per rtp packet.
func (s *StatsInterceptor) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		s.reporter.outgoing.IncrementPackets(1)
		return writer.Write(header, payload, attributes)
	})
}
