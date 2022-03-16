package buffer

import (
	"encoding/binary"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"
)

const (
	ReportDelta = 1e9
)

type pendingPacket struct {
	arrivalTime int64
	packet      []byte
}

type ExtPacket struct {
	Head      bool
	Arrival   int64
	Packet    *rtp.Packet
	Payload   interface{}
	KeyFrame  bool
	RawPacket []byte
}

// Buffer contains all packets
type Buffer struct {
	sync.RWMutex
	bucket     *Bucket
	nacker     *NackQueue
	videoPool  *sync.Pool
	audioPool  *sync.Pool
	codecType  webrtc.RTPCodecType
	extPackets deque.Deque
	pPackets   []pendingPacket
	closeOnce  sync.Once
	mediaSSRC  uint32
	clockRate  uint32
	maxBitrate int64
	lastReport int64
	twccExt    uint8
	audioExt   uint8
	bound      bool
	closed     atomic.Bool
	mime       string

	// supported feedbacks
	remb                             bool
	nack                             bool
	twcc                             bool
	audioLevel                       bool
	latestTSForAudioLevelInitialized bool
	latestTSForAudioLevel            uint32

	lastPacketRead int
	bitrate        Bitrates
	bitrateHelper  Bitrates

	pliThrottle int64

	rtpStats                    *RTPStats
	rrSnapshotId                uint32
	rembSnapshotId              uint32
	connectionQualitySnapshotId uint32

	lastFractionLostToReport uint8 // Last fraction lost from subscribers, should report to publisher; Audio only

	// callbacks
	onClose      func()
	onAudioLevel func(level uint8, durationMs uint32)
	feedbackCB   func([]rtcp.Packet)
	feedbackTWCC func(sn uint16, timeNS int64, marker bool)

	callbacksQueue *utils.OpsQueue

	// logger
	logger logger.Logger
}

// BufferOptions provides configuration options for the buffer
type Options struct {
	MaxBitRate uint64
}

// NewBuffer constructs a new Buffer
func NewBuffer(ssrc uint32, vp, ap *sync.Pool) *Buffer {
	logger := logger.Logger(logger.GetLogger()) // will be reset with correct context via SetLogger
	b := &Buffer{
		mediaSSRC:      ssrc,
		videoPool:      vp,
		audioPool:      ap,
		pliThrottle:    int64(500 * time.Millisecond),
		logger:         logger,
		callbacksQueue: utils.NewOpsQueue(logger),
	}
	b.extPackets.SetMinCapacity(7)
	return b
}

func (b *Buffer) SetLogger(logger logger.Logger) {
	b.logger = logger
	b.callbacksQueue.SetLogger(logger)
}

func (b *Buffer) Bind(params webrtc.RTPParameters, codec webrtc.RTPCodecCapability, o Options) {
	b.Lock()
	defer b.Unlock()
	if b.bound {
		return
	}

	b.rtpStats = NewRTPStats(RTPStatsParams{
		ClockRate: codec.ClockRate,
	})
	b.rrSnapshotId = b.rtpStats.NewSnapshotId()
	b.rembSnapshotId = b.rtpStats.NewSnapshotId()
	b.connectionQualitySnapshotId = b.rtpStats.NewSnapshotId()

	b.callbacksQueue.Start()

	b.clockRate = codec.ClockRate
	b.maxBitrate = int64(o.MaxBitRate)
	b.mime = strings.ToLower(codec.MimeType)

	switch {
	case strings.HasPrefix(b.mime, "audio/"):
		b.codecType = webrtc.RTPCodecTypeAudio
		b.bucket = NewBucket(b.audioPool.Get().(*[]byte))
	case strings.HasPrefix(b.mime, "video/"):
		b.codecType = webrtc.RTPCodecTypeVideo
		b.bucket = NewBucket(b.videoPool.Get().(*[]byte))
	default:
		b.codecType = webrtc.RTPCodecType(0)
	}

	for _, ext := range params.HeaderExtensions {
		if ext.URI == sdp.TransportCCURI {
			b.twccExt = uint8(ext.ID)
			break
		}
	}

	if b.codecType == webrtc.RTPCodecTypeVideo {
		for _, fb := range codec.RTCPFeedback {
			switch fb.Type {
			case webrtc.TypeRTCPFBGoogREMB:
				b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBGoogREMB)
				b.remb = true
			case webrtc.TypeRTCPFBTransportCC:
				b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBTransportCC)
				b.twcc = true
			case webrtc.TypeRTCPFBNACK:
				b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBNACK)
				b.nacker = NewNACKQueue()
				b.nacker.SetRTT(70) // default till it is updated
				b.nack = true
			}
		}
	} else if b.codecType == webrtc.RTPCodecTypeAudio {
		for _, h := range params.HeaderExtensions {
			if h.URI == sdp.AudioLevelURI {
				b.audioLevel = true
				b.audioExt = uint8(h.ID)
			}
		}
	}

	for _, pp := range b.pPackets {
		b.calc(pp.packet, pp.arrivalTime)
	}
	b.pPackets = nil
	b.bound = true

	b.logger.Debugw("NewBuffer", "MaxBitRate", o.MaxBitRate)
}

// Write adds an RTP Packet, out of order, new packet may be arrived later
func (b *Buffer) Write(pkt []byte) (n int, err error) {
	b.Lock()
	defer b.Unlock()

	if b.closed.Load() {
		err = io.EOF
		return
	}

	if !b.bound {
		packet := make([]byte, len(pkt))
		copy(packet, pkt)
		b.pPackets = append(b.pPackets, pendingPacket{
			packet:      packet,
			arrivalTime: time.Now().UnixNano(),
		})
		return
	}

	b.calc(pkt, time.Now().UnixNano())
	return
}

func (b *Buffer) Read(buff []byte) (n int, err error) {
	for {
		if b.closed.Load() {
			err = io.EOF
			return
		}
		b.Lock()
		if b.pPackets != nil && len(b.pPackets) > b.lastPacketRead {
			if len(buff) < len(b.pPackets[b.lastPacketRead].packet) {
				err = ErrBufferTooSmall
				b.Unlock()
				return
			}
			n = len(b.pPackets[b.lastPacketRead].packet)
			copy(buff, b.pPackets[b.lastPacketRead].packet)
			b.lastPacketRead++
			b.Unlock()
			return
		}
		b.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
}

func (b *Buffer) ReadExtended() (*ExtPacket, error) {
	for {
		if b.closed.Load() {
			return nil, io.EOF
		}
		b.Lock()
		if b.extPackets.Len() > 0 {
			extPkt := b.extPackets.PopFront().(*ExtPacket)
			b.Unlock()
			return extPkt, nil
		}
		b.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func (b *Buffer) Close() error {
	b.Lock()
	defer b.Unlock()

	b.closeOnce.Do(func() {
		if b.bucket != nil && b.codecType == webrtc.RTPCodecTypeVideo {
			b.videoPool.Put(b.bucket.src)
		}
		if b.bucket != nil && b.codecType == webrtc.RTPCodecTypeAudio {
			b.audioPool.Put(b.bucket.src)
		}

		b.closed.Store(true)

		if b.rtpStats != nil {
			b.rtpStats.Stop()
			b.logger.Debugw("rtp stats", "stats", b.rtpStats.ToString())
		}

		b.callbacksQueue.Enqueue(b.onClose)
		b.callbacksQueue.Stop()
	})
	return nil
}

func (b *Buffer) OnClose(fn func()) {
	b.onClose = fn
}

func (b *Buffer) SetPLIThrottle(duration int64) {
	b.Lock()
	defer b.Unlock()

	b.pliThrottle = duration
}

func (b *Buffer) SendPLI() {
	b.RLock()
	if b.rtpStats == nil || b.rtpStats.TimeSinceLastPli() < b.pliThrottle {
		b.RUnlock()
		return
	}

	b.rtpStats.UpdatePliAndTime(1)
	b.RUnlock()

	b.logger.Debugw("send pli", "ssrc", b.mediaSSRC)
	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: rand.Uint32(), MediaSSRC: b.mediaSSRC},
	}

	b.callbacksQueue.Enqueue(func() {
		b.feedbackCB(pli)
	})
}

func (b *Buffer) SetRTT(rtt uint32) {
	b.Lock()
	defer b.Unlock()

	if rtt == 0 {
		return
	}

	if b.nacker != nil {
		b.nacker.SetRTT(rtt)
	}

	if b.rtpStats != nil {
		b.rtpStats.UpdateRtt(rtt)
	}
}

func (b *Buffer) calc(pkt []byte, arrivalTime int64) {
	isRTX := false

	pb, err := b.bucket.AddPacket(pkt)
	if err != nil {
		if err != ErrRTXPacket {
			b.logger.Warnw("could not add RTP packet to bucket", err)
			return
		} else {
			isRTX = true
		}
	}

	var p rtp.Packet
	if isRTX {
		err = p.Unmarshal(pkt)
	} else {
		err = p.Unmarshal(pb)
	}
	if err != nil {
		b.logger.Warnw("error unmarshaling RTP packet", err)
		return
	}

	flowState := b.updateStreamState(&p, arrivalTime)

	b.processHeaderExtensions(&p, arrivalTime)

	if isRTX {
		//
		// Run RTX packets through
		//  1. state update - to update stats
		//  2. TWCC just in case remote side is retransmitting an old packet for probing
		//
		// But, do not forward those packets
		//
		return
	}

	ep, spatialLayer, temporalLayer := b.getExtPacket(pb, &p, arrivalTime, flowState.IsHighestSN)
	if ep == nil {
		return
	}
	b.extPackets.PushBack(ep)

	if spatialLayer >= 0 && temporalLayer >= 0 {
		b.bitrateHelper[spatialLayer][temporalLayer] += int64(len(pkt))
	}

	b.doNACKs()

	b.doReports(arrivalTime)
}

func (b *Buffer) updateStreamState(p *rtp.Packet, arrivalTime int64) RTPFlowState {
	flowState := b.rtpStats.Update(&p.Header, len(p.Payload), int(p.PaddingSize), arrivalTime)

	if b.nacker != nil {
		b.nacker.Remove(p.SequenceNumber)

		if flowState.HasLoss {
			for lost := flowState.LossStartInclusive; lost != flowState.LossEndExclusive; lost++ {
				b.nacker.Push(lost)
			}
		}
	}

	return flowState
}

func (b *Buffer) processHeaderExtensions(p *rtp.Packet, arrivalTime int64) {
	// submit to TWCC even if it is a padding only packet. Clients use padding only packets as probes
	// for bandwidth estimation
	if b.twcc {
		if ext := p.GetExtension(b.twccExt); len(ext) > 1 {
			sn := binary.BigEndian.Uint16(ext[0:2])
			marker := p.Marker
			b.callbacksQueue.Enqueue(func() {
				b.feedbackTWCC(sn, arrivalTime, marker)
			})
		}
	}

	if b.audioLevel {
		if !b.latestTSForAudioLevelInitialized {
			b.latestTSForAudioLevelInitialized = true
			b.latestTSForAudioLevel = p.Timestamp
		}
		if e := p.GetExtension(b.audioExt); e != nil && b.onAudioLevel != nil {
			ext := rtp.AudioLevelExtension{}
			if err := ext.Unmarshal(e); err == nil {
				if (p.Timestamp - b.latestTSForAudioLevel) < (1 << 31) {
					duration := (int64(p.Timestamp) - int64(b.latestTSForAudioLevel)) * 1e3 / int64(b.clockRate)
					if duration > 0 {
						b.callbacksQueue.Enqueue(func() {
							b.onAudioLevel(ext.Level, uint32(duration))
						})
					}

					b.latestTSForAudioLevel = p.Timestamp
				}
			}
		}
	}
}

func (b *Buffer) getExtPacket(rawPacket []byte, rtpPacket *rtp.Packet, arrivalTime int64, isHighestSN bool) (*ExtPacket, int32, int32) {
	ep := &ExtPacket{
		Head:      isHighestSN,
		Packet:    rtpPacket,
		Arrival:   arrivalTime,
		RawPacket: rawPacket,
	}

	if len(rtpPacket.Payload) == 0 {
		// padding only packet, nothing else to do
		return ep, -1, -1
	}

	spatialLayer := int32(0)
	temporalLayer := int32(0)
	switch b.mime {
	case "video/vp8":
		vp8Packet := VP8{}
		if err := vp8Packet.Unmarshal(rtpPacket.Payload); err != nil {
			b.logger.Warnw("could not unmarshal VP8 packet", err)
			return nil, -1, -1
		}
		ep.Payload = vp8Packet
		ep.KeyFrame = vp8Packet.IsKeyFrame
		temporalLayer = int32(vp8Packet.TID)
	case "video/h264":
		ep.KeyFrame = IsH264Keyframe(rtpPacket.Payload)
	}
	if ep.KeyFrame {
		b.logger.Debugw("key frame received")
	}

	return ep, spatialLayer, temporalLayer
}

func (b *Buffer) doNACKs() {
	if b.nacker == nil {
		return
	}

	if r, numSeqNumsNacked := b.buildNACKPacket(); r != nil {
		b.callbacksQueue.Enqueue(func() {
			b.feedbackCB(r)
		})
		if b.rtpStats != nil {
			b.rtpStats.UpdateNack(uint32(numSeqNumsNacked))
		}
	}
}

func (b *Buffer) doReports(arrivalTime int64) {
	timeDiff := arrivalTime - b.lastReport
	if timeDiff < ReportDelta {
		return
	}

	b.lastReport = arrivalTime

	//
	// As this happens in the data path, if there are no packets received
	// in an interval, the bitrate will be stuck with the old value.
	// GetBitrate() method in sfu.Receiver uses the availableLayers
	// set by stream tracker to report 0 bitrate if a layer is not available.
	//
	for i := 0; i < len(b.bitrate); i++ {
		for j := 0; j < len(b.bitrate[0]); j++ {
			b.bitrate[i][j] = (8 * b.bitrateHelper[i][j] * int64(ReportDelta)) / timeDiff
			b.bitrateHelper[i][j] = 0
		}
	}

	// RTCP reports
	pkts := b.getRTCP()
	if pkts != nil {
		b.callbacksQueue.Enqueue(func() {
			b.feedbackCB(pkts)
		})
	}
}

func (b *Buffer) buildNACKPacket() ([]rtcp.Packet, int) {
	if nacks, numSeqNumsNacked := b.nacker.Pairs(); len(nacks) > 0 {
		var pkts []rtcp.Packet
		if len(nacks) > 0 {
			pkts = []rtcp.Packet{&rtcp.TransportLayerNack{
				MediaSSRC: b.mediaSSRC,
				Nacks:     nacks,
			}}
		}

		return pkts, numSeqNumsNacked
	}
	return nil, 0
}

func (b *Buffer) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {
	if b.rtpStats == nil {
		return nil
	}

	br := b.Bitrate()
	s := b.rtpStats.SnapshotInfo(b.rembSnapshotId)
	if s == nil {
		return nil
	}

	lostRate := float32(0.0)
	if s.PacketsExpected != 0 {
		lostRate = float32(s.PacketsLost) / float32(s.PacketsExpected)
	}
	if lostRate < 0.02 {
		br = int64(float64(br)*1.09) + 2000
	}
	if lostRate > 0.1 {
		br = int64(float64(br) * float64(1-0.5*lostRate))
	}
	if br > b.maxBitrate {
		br = b.maxBitrate
	}
	if br < 100000 {
		br = 100000
	}

	return &rtcp.ReceiverEstimatedMaximumBitrate{
		Bitrate: float32(br),
		SSRCs:   []uint32{b.mediaSSRC},
	}
}

func (b *Buffer) buildReceptionReport() *rtcp.ReceptionReport {
	if b.rtpStats == nil {
		return nil
	}

	return b.rtpStats.SnapshotRtcpReceptionReport(b.mediaSSRC, b.lastFractionLostToReport, b.rrSnapshotId)
}

func (b *Buffer) SetSenderReportData(rtpTime uint32, ntpTime uint64) {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return
	}

	b.rtpStats.SetRtcpSenderReportData(rtpTime, NtpTime(ntpTime), time.Now())
}

func (b *Buffer) SetLastFractionLostReport(lost uint8) {
	b.lastFractionLostToReport = lost
}

func (b *Buffer) getRTCP() []rtcp.Packet {
	var pkts []rtcp.Packet

	rr := b.buildReceptionReport()
	if rr != nil {
		pkts = append(pkts, &rtcp.ReceiverReport{
			Reports: []rtcp.ReceptionReport{*rr},
		})
	}

	if b.remb && !b.twcc {
		pkts = append(pkts, b.buildREMBPacket())
	}

	return pkts
}

func (b *Buffer) GetPacket(buff []byte, sn uint16) (int, error) {
	b.Lock()
	defer b.Unlock()
	if b.closed.Load() {
		return 0, io.EOF
	}
	return b.bucket.GetPacket(buff, sn)
}

// Bitrate returns the current publisher stream bitrate.
func (b *Buffer) Bitrate() int64 {
	b.RLock()
	defer b.RUnlock()

	bitrate := int64(0)
	for i := 0; i < len(b.bitrate); i++ {
		for j := 0; j < len(b.bitrate[0]); j++ {
			bitrate += b.bitrate[i][j]
		}
	}
	return bitrate
}

// BitrateCumulative returns the current publisher stream bitrate layer accumulated with lower layers.
func (b *Buffer) BitrateCumulative() Bitrates {
	b.RLock()
	defer b.RUnlock()

	var bitrates Bitrates
	for i := len(b.bitrate) - 1; i >= 0; i-- {
		for j := len(b.bitrate) - 1; j >= 0; j-- {
			if b.bitrate[i][j] == 0 {
				continue
			}

			for k := i; k >= 0; k-- {
				for l := j; l >= 0; l-- {
					bitrates[i][j] += b.bitrate[k][l]
				}
			}
		}
	}

	return bitrates
}

func (b *Buffer) OnTransportWideCC(fn func(sn uint16, timeNS int64, marker bool)) {
	b.feedbackTWCC = fn
}

func (b *Buffer) OnFeedback(fn func(fb []rtcp.Packet)) {
	b.feedbackCB = fn
}

func (b *Buffer) OnAudioLevel(fn func(level uint8, durationMs uint32)) {
	b.onAudioLevel = fn
}

// GetMediaSSRC returns the associated SSRC of the RTP stream
func (b *Buffer) GetMediaSSRC() uint32 {
	return b.mediaSSRC
}

// GetClockRate returns the RTP clock rate
func (b *Buffer) GetClockRate() uint32 {
	return b.clockRate
}

func (b *Buffer) GetStats() *StreamStatsWithLayers {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return nil
	}

	stats := b.rtpStats.ToProto()
	if stats == nil {
		return nil
	}

	layers := make(map[int]LayerStats)
	layers[0] = LayerStats{
		TotalPackets: stats.Packets + stats.PacketsDuplicate + stats.PacketsPadding,
		TotalBytes:   stats.Bytes + stats.BytesDuplicate + stats.BytesPadding,
		TotalFrames:  stats.Frames,
	}

	return &StreamStatsWithLayers{
		RTPStats: stats,
		Layers:   layers,
	}
}

func (b *Buffer) GetQualityInfo() *RTPSnapshotInfo {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return nil
	}

	return b.rtpStats.SnapshotInfo(b.connectionQualitySnapshotId)
}
