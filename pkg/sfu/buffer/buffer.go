package buffer

import (
	"encoding/binary"
	"io"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gammazero/deque"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
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
	closed     atomicBool
	mime       string

	// supported feedbacks
	remb                             bool
	nack                             bool
	twcc                             bool
	audioLevel                       bool
	latestTSForAudioLevelInitialized bool
	latestTSForAudioLevel            uint32

	lastPacketRead int
	bitrate        atomic.Value
	bitrateHelper  [4]int64
	lastSRNTPTime  uint64
	lastSRRTPTime  uint32
	lastSRRecv     int64 // Represents wall clock of the most recent sender report arrival
	lastTransit    uint32

	pliThrottle int64
	lastPli     int64

	started    bool
	stats      StreamStats
	rrSnapshot *receiverReportSnapshot

	highestSN uint16
	cycle     uint16

	lastFractionLostToReport uint8 // Last fraction lost from subscribers, should report to publisher; Audio only

	// callbacks
	onClose      func()
	onAudioLevel func(level uint8, durationMs uint32)
	feedbackCB   func([]rtcp.Packet)
	feedbackTWCC func(sn uint16, timeNS int64, marker bool)
	callbackOps  chan func()

	// logger
	logger logger.Logger
}

type receiverReportSnapshot struct {
	extHighestSeqNum uint32
	packetsLost      uint32
	lastLossRate     float32
}

// BufferOptions provides configuration options for the buffer
type Options struct {
	MaxBitRate uint64
}

// NewBuffer constructs a new Buffer
func NewBuffer(ssrc uint32, vp, ap *sync.Pool) *Buffer {
	b := &Buffer{
		mediaSSRC:   ssrc,
		videoPool:   vp,
		audioPool:   ap,
		pliThrottle: int64(500 * time.Millisecond),
		logger:      logger.Logger(logger.GetLogger()), // will be reset with correct context via SetLogger
		callbackOps: make(chan func(), 50),
	}
	b.bitrate.Store(make([]int64, len(b.bitrateHelper)))
	b.extPackets.SetMinCapacity(7)
	return b
}

func (b *Buffer) SetLogger(logger logger.Logger) {
	b.logger = logger
}

func (b *Buffer) Bind(params webrtc.RTPParameters, codec webrtc.RTPCodecCapability, o Options) {
	b.Lock()
	defer b.Unlock()
	if b.bound {
		return
	}

	go b.doCallbacks()

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

func (b *Buffer) doCallbacks() {
	for op := range b.callbackOps {
		op()
	}
}

// Write adds an RTP Packet, out of order, new packet may be arrived later
func (b *Buffer) Write(pkt []byte) (n int, err error) {
	b.Lock()
	defer b.Unlock()

	if b.closed.get() {
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
		if b.closed.get() {
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
		if b.closed.get() {
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
		b.closed.set(true)
		b.onClose()
		close(b.callbackOps)
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
	now := time.Now().UnixNano()

	b.Lock()
	throttled := now-b.lastPli < b.pliThrottle
	if throttled {
		b.Unlock()
		return
	}
	b.lastPli = now
	b.stats.TotalPLIs++
	b.Unlock()

	b.logger.Debugw("send pli", "ssrc", b.mediaSSRC)
	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: rand.Uint32(), MediaSSRC: b.mediaSSRC},
	}

	b.callbackOps <- func() {
		b.feedbackCB(pli)
	}
}

func (b *Buffer) SetRTT(rtt uint32) {
	b.Lock()
	defer b.Unlock()

	b.stats.RTT = rtt

	if b.nacker != nil && rtt != 0 {
		b.nacker.SetRTT(rtt)
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

	b.updateStreamState(&p, len(pkt), arrivalTime, isRTX)

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

	ep, temporalLayer := b.getExtPacket(pb, &p, arrivalTime)
	if ep == nil {
		return
	}
	b.extPackets.PushBack(ep)

	if temporalLayer >= 0 {
		b.bitrateHelper[temporalLayer] += int64(len(pkt))
	}

	b.doNACKs()

	b.doReports(arrivalTime)
}

func (b *Buffer) updateStreamState(p *rtp.Packet, pktSize int, arrivalTime int64, isRTX bool) {
	sn := p.SequenceNumber

	if !b.started {
		b.started = true
		b.highestSN = sn

		b.lastReport = arrivalTime

		b.rrSnapshot = &receiverReportSnapshot{
			extHighestSeqNum: uint32(sn) - 1,
			packetsLost:      0,
			lastLossRate:     0.0,
		}
	} else {
		diff := sn - b.highestSN
		if diff > (1 << 15) {
			if !isRTX && b.stats.TotalPacketsLost != 0 {
				b.stats.TotalPacketsLost--
			}

			// out-of-order, remove it from nack queue
			if b.nacker != nil {
				b.nacker.Remove(sn)
			}
		} else {
			if diff > 1 {
				b.stats.TotalPacketsLost += (uint32(diff) - 1)
				if b.nacker != nil {
					for lost := b.highestSN + 1; lost != sn; lost++ {
						b.nacker.Push(lost)
					}
				}
			}

			if sn < b.highestSN {
				b.cycle++
			}

			b.highestSN = sn
		}
	}

	switch {
	case isRTX:
		b.stats.TotalRetransmitPackets++
		b.stats.TotalRetransmitBytes += uint64(pktSize)
	case len(p.Payload) == 0:
		b.stats.TotalPaddingPackets++
		b.stats.TotalPaddingBytes += uint64(pktSize)
	default:
		b.stats.TotalPrimaryPackets++
		b.stats.TotalPrimaryBytes += uint64(pktSize)
		if p.Marker {
			b.stats.TotalFrames++
		}
	}

	if !isRTX {
		// jitter
		arrival := uint32(arrivalTime / 1e6 * int64(b.clockRate/1e3))
		transit := arrival - p.Timestamp
		if b.lastTransit != 0 {
			d := int32(transit - b.lastTransit)
			if d < 0 {
				d = -d
			}
			b.stats.Jitter += (float64(d) - b.stats.Jitter) / 16
		}
		b.lastTransit = transit
	}
}

func (b *Buffer) processHeaderExtensions(p *rtp.Packet, arrivalTime int64) {
	// submit to TWCC even if it is a padding only packet. Clients use padding only packets as probes
	// for bandwidth estimation
	if b.twcc {
		if ext := p.GetExtension(b.twccExt); len(ext) > 1 {
			sn := binary.BigEndian.Uint16(ext[0:2])
			marker := p.Marker
			b.callbackOps <- func() {
				b.feedbackTWCC(sn, arrivalTime, marker)
			}
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
						b.callbackOps <- func() {
							b.onAudioLevel(ext.Level, uint32(duration))
						}
					}

					b.latestTSForAudioLevel = p.Timestamp
				}
			}
		}
	}
}

func (b *Buffer) getExtPacket(rawPacket []byte, rtpPacket *rtp.Packet, arrivalTime int64) (*ExtPacket, int32) {
	ep := &ExtPacket{
		Head:      rtpPacket.SequenceNumber == b.highestSN,
		Packet:    rtpPacket,
		Arrival:   arrivalTime,
		RawPacket: rawPacket,
	}

	if len(rtpPacket.Payload) == 0 {
		// padding only packet, nothing else to do
		return ep, -1
	}

	temporalLayer := int32(0)
	switch b.mime {
	case "video/vp8":
		vp8Packet := VP8{}
		if err := vp8Packet.Unmarshal(rtpPacket.Payload); err != nil {
			b.logger.Warnw("could not unmarshal VP8 packet", err)
			return nil, -1
		}
		ep.Payload = vp8Packet
		ep.KeyFrame = vp8Packet.IsKeyFrame
		temporalLayer = int32(vp8Packet.TID)
		if ep.KeyFrame {
			b.logger.Debugw("SA_DEBUG key frame", "ssrc", b.mediaSSRC)	// REMOVE
		}
	case "video/h264":
		ep.KeyFrame = IsH264Keyframe(rtpPacket.Payload)
	}

	return ep, temporalLayer
}

func (b *Buffer) doNACKs() {
	if b.nacker == nil {
		return
	}

	if r, numSeqNumsNacked := b.buildNACKPacket(); r != nil {
		b.callbackOps <- func() {
			b.feedbackCB(r)
		}
		b.stats.TotalNACKs += uint32(numSeqNumsNacked)
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
	bitrates, ok := b.bitrate.Load().([]int64)
	if !ok {
		bitrates = make([]int64, len(b.bitrateHelper))
	}
	for i := 0; i < len(b.bitrateHelper); i++ {
		br := (8 * b.bitrateHelper[i] * int64(ReportDelta)) / timeDiff
		bitrates[i] = br
		b.bitrateHelper[i] = 0
	}
	b.bitrate.Store(bitrates)

	// RTCP reports
	pkts := b.getRTCP()
	b.callbackOps <- func() {
		b.feedbackCB(pkts)
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
	br := b.Bitrate()

	lostRate := float32(0)
	if b.rrSnapshot != nil {
		lostRate = b.rrSnapshot.lastLossRate
	}

	if lostRate < 0.02 {
		br = int64(float64(br)*1.09) + 2000
	}
	if lostRate > .1 {
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
	if b.rrSnapshot == nil {
		return nil
	}

	extHighestSeqNum := (uint32(b.cycle) << 16) | uint32(b.highestSN)
	expectedInInterval := extHighestSeqNum - b.rrSnapshot.extHighestSeqNum
	if expectedInInterval == 0 {
		return nil
	}

	lostInInterval := b.stats.TotalPacketsLost - b.rrSnapshot.packetsLost
	if int32(lostInInterval) < 0 {
		// could happen if retransmitted packets arrive and make received greater than expected
		lostInInterval = 0
	}

	lossRate := float32(lostInInterval) / float32(expectedInInterval)
	fracLost := uint8(lossRate * 256.0)
	if b.lastFractionLostToReport > fracLost {
		// max of fraction lost from all subscribers is bigger than sfu received, use it.
		fracLost = b.lastFractionLostToReport
	}

	var dlsr uint32
	if b.lastSRRecv != 0 {
		delayMS := uint32((time.Now().UnixNano() - b.lastSRRecv) / 1e6)
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}

	b.rrSnapshot = &receiverReportSnapshot{
		extHighestSeqNum: extHighestSeqNum,
		packetsLost:      b.stats.TotalPacketsLost,
		lastLossRate:     lossRate,
	}

	return &rtcp.ReceptionReport{
		SSRC:               b.mediaSSRC,
		FractionLost:       fracLost,
		TotalLost:          b.stats.TotalPacketsLost,
		LastSequenceNumber: extHighestSeqNum,
		Jitter:             uint32(b.stats.Jitter),
		LastSenderReport:   uint32(b.lastSRNTPTime >> 16),
		Delay:              dlsr,
	}
}

func (b *Buffer) SetSenderReportData(rtpTime uint32, ntpTime uint64) {
	b.Lock()
	b.lastSRRTPTime = rtpTime
	b.lastSRNTPTime = ntpTime
	b.lastSRRecv = time.Now().UnixNano()
	b.Unlock()
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
	if b.closed.get() {
		return 0, io.EOF
	}
	return b.bucket.GetPacket(buff, sn)
}

// Bitrate returns the current publisher stream bitrate.
func (b *Buffer) Bitrate() int64 {
	bitrates, ok := b.bitrate.Load().([]int64)
	bitrate := int64(0)
	if ok {
		for _, b := range bitrates {
			bitrate += b
		}
	}
	return bitrate
}

// BitrateTemporalCumulative returns the current publisher stream bitrate temporal layer accumulated with lower temporal layers.
func (b *Buffer) BitrateTemporalCumulative() []int64 {
	bitrates, ok := b.bitrate.Load().([]int64)
	if !ok {
		return make([]int64, len(b.bitrateHelper))
	}

	// copy and process
	brs := make([]int64, len(bitrates))
	copy(brs, bitrates)

	for i := len(brs) - 1; i >= 1; i-- {
		if brs[i] != 0 {
			for j := i - 1; j >= 0; j-- {
				brs[i] += brs[j]
			}
		}
	}

	return brs
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

// GetSenderReportData returns the rtp, ntp and nanos of the last sender report
func (b *Buffer) GetSenderReportData() (rtpTime uint32, ntpTime uint64, lastReceivedTimeInNanosSinceEpoch int64) {
	rtpTime = atomic.LoadUint32(&b.lastSRRTPTime)
	ntpTime = atomic.LoadUint64(&b.lastSRNTPTime)
	lastReceivedTimeInNanosSinceEpoch = atomic.LoadInt64(&b.lastSRRecv)

	return rtpTime, ntpTime, lastReceivedTimeInNanosSinceEpoch
}

func (b *Buffer) GetStats() *StreamStatsWithLayers {
	b.RLock()
	defer b.RUnlock()

	layers := make(map[int]LayerStats)
	layers[0] = LayerStats{
		TotalPackets: b.stats.TotalPrimaryPackets + b.stats.TotalRetransmitPackets + b.stats.TotalPaddingPackets,
		TotalBytes:   b.stats.TotalPrimaryBytes + b.stats.TotalRetransmitBytes + b.stats.TotalPaddingBytes,
		TotalFrames:  b.stats.TotalFrames,
	}

	return &StreamStatsWithLayers{
		StreamStats: b.stats,
		Layers:      layers,
	}
}
