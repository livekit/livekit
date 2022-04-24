package buffer

import (
	"encoding/binary"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/audio"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
)

const (
	ReportDelta = 1e9
)

type pendingPacket struct {
	arrivalTime int64
	packet      []byte
}

type ExtPacket struct {
	Head          bool
	Arrival       int64
	Packet        *rtp.Packet
	Payload       interface{}
	KeyFrame      bool
	RawPacket     []byte
	SpatialLayer  int32
	TemporalLayer int32
}

// Buffer contains all packets
type Buffer struct {
	sync.RWMutex
	bucket        *Bucket
	nacker        *NackQueue
	videoPool     *sync.Pool
	audioPool     *sync.Pool
	codecType     webrtc.RTPCodecType
	extPackets    deque.Deque
	pPackets      []pendingPacket
	closeOnce     sync.Once
	mediaSSRC     uint32
	clockRate     uint32
	lastReport    int64
	twccExt       uint8
	audioLevelExt uint8
	bound         bool
	closed        atomic.Bool
	mime          string

	// supported feedbacks
	latestTSForAudioLevelInitialized bool
	latestTSForAudioLevel            uint32

	twcc             *twcc.Responder
	audioLevelParams audio.AudioLevelParams
	audioLevel       *audio.AudioLevel

	lastPacketRead int

	pliThrottle int64

	rtpStats                    *RTPStats
	rrSnapshotId                uint32
	connectionQualitySnapshotId uint32
	deltaStatsSnapshotId        uint32

	lastFractionLostToReport uint8 // Last fraction lost from subscribers, should report to publisher; Audio only

	// callbacks
	onClose        func()
	onRtcpFeedback func([]rtcp.Packet)

	// logger
	logger logger.Logger
}

// BufferOptions provides configuration options for the buffer
type Options struct {
	MaxBitRate uint64
}

// NewBuffer constructs a new Buffer
func NewBuffer(ssrc uint32, vp, ap *sync.Pool) *Buffer {
	l := logger.GetDefaultLogger() // will be reset with correct context via SetLogger
	b := &Buffer{
		mediaSSRC:   ssrc,
		videoPool:   vp,
		audioPool:   ap,
		pliThrottle: int64(500 * time.Millisecond),
		logger:      l,
	}
	b.extPackets.SetMinCapacity(7)
	return b
}

func (b *Buffer) SetLogger(logger logger.Logger) {
	b.Lock()
	defer b.Unlock()

	b.logger = logger
	if b.rtpStats != nil {
		b.rtpStats.SetLogger(logger)
	}
}

func (b *Buffer) SetTWCC(twcc *twcc.Responder) {
	b.Lock()
	defer b.Unlock()

	b.twcc = twcc
}

func (b *Buffer) SetAudioLevelParams(audioLevelParams audio.AudioLevelParams) {
	b.Lock()
	defer b.Unlock()

	b.audioLevelParams = audioLevelParams
}

func (b *Buffer) Bind(params webrtc.RTPParameters, codec webrtc.RTPCodecCapability) {
	b.Lock()
	defer b.Unlock()
	if b.bound {
		return
	}

	b.rtpStats = NewRTPStats(RTPStatsParams{
		ClockRate: codec.ClockRate,
		Logger:    b.logger,
	})
	b.rrSnapshotId = b.rtpStats.NewSnapshotId()
	b.connectionQualitySnapshotId = b.rtpStats.NewSnapshotId()
	b.deltaStatsSnapshotId = b.rtpStats.NewSnapshotId()

	b.clockRate = codec.ClockRate
	b.lastReport = time.Now().UnixNano()
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

	if b.codecType == webrtc.RTPCodecTypeVideo {
		for _, fb := range codec.RTCPFeedback {
			switch fb.Type {
			case webrtc.TypeRTCPFBGoogREMB:
				b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBGoogREMB)
				b.logger.Warnw("REMB not supported, RTCP feedback will not be generated", nil)
			case webrtc.TypeRTCPFBTransportCC:
				b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBTransportCC)
				for _, ext := range params.HeaderExtensions {
					if ext.URI == sdp.TransportCCURI {
						b.twccExt = uint8(ext.ID)
						break
					}
				}
			case webrtc.TypeRTCPFBNACK:
				b.logger.Debugw("Setting feedback", "type", webrtc.TypeRTCPFBNACK)
				b.nacker = NewNACKQueue()
				b.nacker.SetRTT(70) // default till it is updated
			}
		}
	} else if b.codecType == webrtc.RTPCodecTypeAudio {
		for _, h := range params.HeaderExtensions {
			if h.URI == sdp.AudioLevelURI {
				b.audioLevelExt = uint8(h.ID)
				b.audioLevel = audio.NewAudioLevel(b.audioLevelParams)
			}
		}
	}

	for _, pp := range b.pPackets {
		b.calc(pp.packet, pp.arrivalTime)
	}
	b.pPackets = nil
	b.bound = true
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

		if b.onClose != nil {
			b.onClose()
		}
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

	if b.onRtcpFeedback != nil {
		b.onRtcpFeedback(pli)
	}
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
	pb, err := b.bucket.AddPacket(pkt)
	if err != nil {
		//
		// Even when erroring, do
		//  1. state update
		//  2. TWCC just in case remote side is retransmitting an old packet for probing
		//
		// But, do not forward those packets
		//
		var rtpPacket rtp.Packet
		if uerr := rtpPacket.Unmarshal(pkt); uerr == nil {
			b.updateStreamState(&rtpPacket, arrivalTime)
			b.processHeaderExtensions(&rtpPacket, arrivalTime)
		}

		if err != ErrRTXPacket {
			b.logger.Warnw("could not add RTP packet to bucket", err)
		}
		return
	}

	var p rtp.Packet
	err = p.Unmarshal(pb)
	if err != nil {
		b.logger.Warnw("error unmarshaling RTP packet", err)
		return
	}

	flowState := b.updateStreamState(&p, arrivalTime)
	b.processHeaderExtensions(&p, arrivalTime)

	ep := b.getExtPacket(pb, &p, arrivalTime, flowState.IsHighestSN)
	if ep == nil {
		return
	}
	b.extPackets.PushBack(ep)

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
	if b.twcc != nil && b.twccExt != 0 {
		if ext := p.GetExtension(b.twccExt); ext != nil {
			b.twcc.Push(binary.BigEndian.Uint16(ext[0:2]), arrivalTime, p.Marker)
		}
	}

	if b.audioLevelExt != 0 {
		if !b.latestTSForAudioLevelInitialized {
			b.latestTSForAudioLevelInitialized = true
			b.latestTSForAudioLevel = p.Timestamp
		}
		if e := p.GetExtension(b.audioLevelExt); e != nil {
			ext := rtp.AudioLevelExtension{}
			if err := ext.Unmarshal(e); err == nil {
				if (p.Timestamp - b.latestTSForAudioLevel) < (1 << 31) {
					duration := (int64(p.Timestamp) - int64(b.latestTSForAudioLevel)) * 1e3 / int64(b.clockRate)
					if duration > 0 {
						b.audioLevel.Observe(ext.Level, uint32(duration))
					}

					b.latestTSForAudioLevel = p.Timestamp
				}
			}
		}
	}
}

func (b *Buffer) getExtPacket(rawPacket []byte, rtpPacket *rtp.Packet, arrivalTime int64, isHighestSN bool) *ExtPacket {
	ep := &ExtPacket{
		Head:          isHighestSN,
		Packet:        rtpPacket,
		Arrival:       arrivalTime,
		RawPacket:     rawPacket,
		SpatialLayer:  -1,
		TemporalLayer: -1,
	}

	if len(rtpPacket.Payload) == 0 {
		// padding only packet, nothing else to do
		return ep
	}

	ep.TemporalLayer = 0
	switch b.mime {
	case "video/vp8":
		vp8Packet := VP8{}
		if err := vp8Packet.Unmarshal(rtpPacket.Payload); err != nil {
			b.logger.Warnw("could not unmarshal VP8 packet", err)
			return nil
		}
		ep.Payload = vp8Packet
		ep.KeyFrame = vp8Packet.IsKeyFrame
		ep.TemporalLayer = int32(vp8Packet.TID)
	case "video/h264":
		ep.KeyFrame = IsH264Keyframe(rtpPacket.Payload)

	case "video/vp9":
		ep.KeyFrame = IsVp9Keyframe(rtpPacket.Payload)
	case "video/av1":
		ep.KeyFrame = IsAV1Keyframe(rtpPacket.Payload)
	}
	if ep.KeyFrame {
		if b.rtpStats != nil {
			b.rtpStats.UpdateKeyFrame(1)
		}
	}

	return ep
}

func (b *Buffer) doNACKs() {
	if b.nacker == nil {
		return
	}

	if r, numSeqNumsNacked := b.buildNACKPacket(); r != nil {
		if b.onRtcpFeedback != nil {
			b.onRtcpFeedback(r)
		}
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

	// RTCP reports
	pkts := b.getRTCP()
	if pkts != nil && b.onRtcpFeedback != nil {
		b.onRtcpFeedback(pkts)
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

func (b *Buffer) OnRtcpFeedback(fn func(fb []rtcp.Packet)) {
	b.onRtcpFeedback = fn
}

// GetMediaSSRC returns the associated SSRC of the RTP stream
func (b *Buffer) GetMediaSSRC() uint32 {
	return b.mediaSSRC
}

// GetClockRate returns the RTP clock rate
func (b *Buffer) GetClockRate() uint32 {
	return b.clockRate
}

func (b *Buffer) GetStats() *livekit.RTPStats {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return nil
	}

	return b.rtpStats.ToProto()
}

func (b *Buffer) GetQualityInfo() *RTPSnapshotInfo {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return nil
	}

	return b.rtpStats.SnapshotInfo(b.connectionQualitySnapshotId)
}

func (b *Buffer) GetDeltaStats() *StreamStatsWithLayers {
	b.RLock()
	defer b.RUnlock()

	if b.rtpStats == nil {
		return nil
	}

	deltaStats := b.rtpStats.DeltaInfo(b.deltaStatsSnapshotId)
	if deltaStats == nil {
		return nil
	}

	layers := make(map[int]LayerStats)
	layers[0] = LayerStats{
		Packets: deltaStats.Packets + deltaStats.PacketsDuplicate + deltaStats.PacketsPadding,
		Bytes:   deltaStats.Bytes + deltaStats.BytesDuplicate + deltaStats.BytesPadding,
		Frames:  deltaStats.Frames,
	}

	return &StreamStatsWithLayers{
		RTPStats: deltaStats,
		Layers:   layers,
	}
}

func (b *Buffer) GetAudioLevel() (float64, bool) {
	b.RLock()
	defer b.RUnlock()

	if b.audioLevel == nil {
		return 0, false
	}

	return b.audioLevel.GetLevel()
}
