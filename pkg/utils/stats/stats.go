package stats

import (
	"io"
	"time"

	linuxproc "github.com/c9s/goprocinfo/linux"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/transport/packetio"
)

const livekitNamespace = "livekit"

var (
	promLabels = []string{"direction"}
)

func init() {
	initPacketStats()
	initRoomStatsReporter()
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

func UpdateCurrentNodeStats(nodeStats *livekit.NodeStats) (err error) {
	updatedAtPrevious := nodeStats.UpdatedAt
	nodeStats.UpdatedAt = time.Now().Unix()
	secondsSinceLastUpdate := nodeStats.UpdatedAt - updatedAtPrevious

	err = updateCurrentNodeSystemStats(nodeStats)
	updateCurrentNodeRoomStats(nodeStats)
	updateCurrentNodePacketStats(nodeStats, secondsSinceLastUpdate)

	return
}

func updateCurrentNodeSystemStats(nodeStats *livekit.NodeStats) (err error) {
	var cpuInfo *linuxproc.CPUInfo
	if cpuInfo, err = linuxproc.ReadCPUInfo("/proc/cpuinfo"); err == nil {
		nodeStats.NumCpus = uint32(cpuInfo.NumCPU())
	}

	var loadAvg *linuxproc.LoadAvg
	if loadAvg, err = linuxproc.ReadLoadAvg("/proc/loadavg"); err == nil {
		nodeStats.LoadAvgLast1Min = float32(loadAvg.Last1Min)
		nodeStats.LoadAvgLast5Min = float32(loadAvg.Last5Min)
		nodeStats.LoadAvgLast15Min = float32(loadAvg.Last15Min)
	}

	return
}
