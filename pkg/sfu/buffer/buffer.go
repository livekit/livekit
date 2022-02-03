package buffer

import (
	"encoding/binary"
	"io"
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
	Packet    rtp.Packet
	Payload   interface{}
	KeyFrame  bool
	RawPacket []byte
}

// Buffer contains all packets
type Buffer struct {
	sync.Mutex
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
	remb       bool
	nack       bool
	twcc       bool
	audioLevel bool

	lastPacketRead     int
	bitrate            atomic.Value
	bitrateHelper      [4]int64
	lastSRNTPTime      uint64
	lastSRRTPTime      uint32
	lastSRRecv         int64 // Represents wall clock of the most recent sender report arrival
	baseSN             uint16
	highestSN          uint16
	cycle              uint16
	lastRtcpPacketTime int64 // Time the last RTCP packet was received.
	lastRtcpSrTime     int64 // Time the last RTCP SR was received. Required for DLSR computation.
	lastTransit        uint32

	stats      Stats
	rrSnapshot *receiverReportSnapshot

	latestTimestamp          uint32 // latest received RTP timestamp on packet
	latestTimestampTime      int64  // Time of the latest timestamp (in nanos since unix epoch)
	lastFractionLostToReport uint8  // Last fraction lost from subscribers, should report to publisher; Audio only

	// callbacks
	onClose      func()
	onAudioLevel func(level uint8, durationMs uint32)
	feedbackCB   func([]rtcp.Packet)
	feedbackTWCC func(sn uint16, timeNS int64, marker bool)

	// logger
	logger logger.Logger
}

type Stats struct {
	PacketCount uint32 // Number of packets received from this source.
	TotalBytes  uint64
	Jitter      float64 // An estimate of the statistical variance of the RTP data packet inter-arrival time.
}

type receiverReportSnapshot struct {
	extSeqNum       uint32
	packetsReceived uint32
	packetsLost     uint32
}

// BufferOptions provides configuration options for the buffer
type Options struct {
	MaxBitRate uint64
}

// NewBuffer constructs a new Buffer
func NewBuffer(ssrc uint32, vp, ap *sync.Pool) *Buffer {
	b := &Buffer{
		mediaSSRC: ssrc,
		videoPool: vp,
		audioPool: ap,
		logger:    logger.Logger(logger.GetLogger()), // will be reset with correct context via SetLogger
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
				b.logger.Infow("Setting feedback", "type", webrtc.TypeRTCPFBGoogREMB)
				b.remb = true
			case webrtc.TypeRTCPFBTransportCC:
				b.logger.Infow("Setting feedback", "type", webrtc.TypeRTCPFBTransportCC)
				b.twcc = true
			case webrtc.TypeRTCPFBNACK:
				b.logger.Infow("Setting feedback", "type", webrtc.TypeRTCPFBNACK)
				b.nacker = NewNACKQueue()
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

	b.logger.Infow("NewBuffer", "MaxBitRate", o.MaxBitRate)
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
	})
	return nil
}

func (b *Buffer) OnClose(fn func()) {
	b.onClose = fn
}

func (b *Buffer) calc(pkt []byte, arrivalTime int64) {
	sn := binary.BigEndian.Uint16(pkt[2:4])

	if b.stats.PacketCount == 0 {
		b.highestSN = sn - 1
		b.lastReport = arrivalTime

		b.rrSnapshot = &receiverReportSnapshot{
			extSeqNum:       uint32(sn) - 1,
			packetsReceived: 0,
			packetsLost:     0,
		}
	}

	diff := sn - b.highestSN
	if diff > (1 << 15) {
		// out-of-order, remove it from nack queue
		if b.nacker != nil {
			b.nacker.Remove(sn)
		}
	} else {
		if b.nacker != nil && diff > 1 {
			for lost := b.highestSN + 1; lost != sn; lost++ {
				b.nacker.Push(lost)
			}
		}

		if sn < b.highestSN && b.stats.PacketCount > 0 {
			b.cycle++
		}

		b.highestSN = sn
	}

	headPkt := sn == b.highestSN
	var p rtp.Packet
	pb, err := b.bucket.AddPacket(pkt, sn, headPkt)
	if err != nil {
		if err == ErrRTXPacket {
			return
		}
		return
	}
	if err = p.Unmarshal(pb); err != nil {
		return
	}

	// submit to TWCC even if it is a padding only packet. Clients use padding only packets as probes
	// for bandwidth estimation
	if b.twcc {
		if ext := p.GetExtension(b.twccExt); len(ext) > 1 {
			b.feedbackTWCC(binary.BigEndian.Uint16(ext[0:2]), arrivalTime, p.Marker)
		}
	}

	b.stats.TotalBytes += uint64(len(pkt))
	b.stats.PacketCount++

	ep := ExtPacket{
		Head:      headPkt,
		Packet:    p,
		Arrival:   arrivalTime,
		RawPacket: pb,
	}

	if len(p.Payload) == 0 {
		// padding only packet, nothing else to do
		b.extPackets.PushBack(&ep)
		return
	}

	temporalLayer := int32(0)
	switch b.mime {
	case "video/vp8":
		vp8Packet := VP8{}
		if err := vp8Packet.Unmarshal(p.Payload); err != nil {
			return
		}
		ep.Payload = vp8Packet
		ep.KeyFrame = vp8Packet.IsKeyFrame
		temporalLayer = int32(vp8Packet.TID)
	case "video/h264":
		ep.KeyFrame = IsH264Keyframe(p.Payload)
	}

	b.extPackets.PushBack(&ep)

	// if first time update or the timestamp is later (factoring timestamp wrap around)
	latestTimestamp := atomic.LoadUint32(&b.latestTimestamp)
	latestTimestampTimeInNanosSinceEpoch := atomic.LoadInt64(&b.latestTimestampTime)
	if (latestTimestampTimeInNanosSinceEpoch == 0) || IsLaterTimestamp(p.Timestamp, latestTimestamp) {
		atomic.StoreUint32(&b.latestTimestamp, p.Timestamp)
		atomic.StoreInt64(&b.latestTimestampTime, arrivalTime)
	}

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

	if b.audioLevel {
		if e := p.GetExtension(b.audioExt); e != nil && b.onAudioLevel != nil {
			ext := rtp.AudioLevelExtension{}
			if err := ext.Unmarshal(e); err == nil {
				duration := (int64(p.Timestamp) - int64(latestTimestamp)) * 1e3 / int64(b.clockRate)
				if duration > 0 {
					b.onAudioLevel(ext.Level, uint32(duration))
				}
			}
		}
	}

	if b.nacker != nil {
		if r := b.buildNACKPacket(); r != nil {
			b.feedbackCB(r)
		}
	}

	b.bitrateHelper[temporalLayer] += int64(len(pkt))

	timeDiff := arrivalTime - b.lastReport
	if timeDiff >= ReportDelta {
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
		b.feedbackCB(b.getRTCP())
		b.lastReport = arrivalTime
	}
}

func (b *Buffer) buildNACKPacket() []rtcp.Packet {
	if nacks := b.nacker.Pairs(); len(nacks) > 0 {
		var pkts []rtcp.Packet
		if len(nacks) > 0 {
			pkts = []rtcp.Packet{&rtcp.TransportLayerNack{
				MediaSSRC: b.mediaSSRC,
				Nacks:     nacks,
			}}
		}

		return pkts
	}
	return nil
}

func (b *Buffer) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {
	br := b.Bitrate()

	extMaxSeq := (uint32(b.cycle) << 16) | uint32(b.highestSN)
	expectedInInterval := extMaxSeq - b.rrSnapshot.extSeqNum
	receivedInInterval := b.stats.PacketCount - b.rrSnapshot.packetsReceived
	lostInInterval := expectedInInterval - receivedInInterval
	if int(lostInInterval) < 0 {
		// could happen if retransmitted packets arrive and make received greater than expected
		lostInInterval = 0
	}
	lostRate := float32(0)
	if expectedInInterval != 0 {
		lostRate = float32(lostInInterval) / float32(expectedInInterval)
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
	b.stats.TotalBytes = 0

	return &rtcp.ReceiverEstimatedMaximumBitrate{
		Bitrate: float32(br),
		SSRCs:   []uint32{b.mediaSSRC},
	}
}

func (b *Buffer) buildReceptionReport() *rtcp.ReceptionReport {
	if b.rrSnapshot == nil {
		return nil
	}

	extMaxSeq := (uint32(b.cycle) << 16) | uint32(b.highestSN)
	expectedInInterval := extMaxSeq - b.rrSnapshot.extSeqNum
	if expectedInInterval == 0 {
		return nil
	}

	receivedInInterval := b.stats.PacketCount - b.rrSnapshot.packetsReceived
	lostInInterval := expectedInInterval - receivedInInterval
	if int(lostInInterval) < 0 {
		// could happen if retransmitted packets arrive and make received greater than expected
		lostInInterval = 0
	}

	fracLost := uint8((float32(lostInInterval) / float32(expectedInInterval)) * 256.0)
	if b.lastFractionLostToReport > fracLost {
		// max of fraction lost from all subscribers is bigger than sfu received, use it.
		fracLost = b.lastFractionLostToReport
	}

	totalLost := b.rrSnapshot.packetsLost + lostInInterval

	var dlsr uint32
	if b.lastSRRecv != 0 {
		delayMS := uint32((time.Now().UnixNano() - b.lastSRRecv) / 1e6)
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}

	b.rrSnapshot = &receiverReportSnapshot{
		extSeqNum:       extMaxSeq,
		packetsReceived: b.stats.PacketCount,
		packetsLost:     totalLost,
	}

	return &rtcp.ReceptionReport{
		SSRC:               b.mediaSSRC,
		FractionLost:       fracLost,
		TotalLost:          totalLost,
		LastSequenceNumber: extMaxSeq,
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

// GetStats returns the raw statistics about a particular buffer state
func (b *Buffer) GetStats() (stats Stats) {
	b.Lock()
	stats = b.stats
	b.Unlock()
	return
}

// Used only in tests
func (b *Buffer) SetStatsTestOnly(stats Stats) {
	b.Lock()
	b.stats = stats
	b.Unlock()
}

// IsLaterTimestamp returns true if timestamp1 is later in time than timestamp2 factoring in timestamp wrap-around
func IsLaterTimestamp(timestamp1 uint32, timestamp2 uint32) bool {
	return (timestamp1 - timestamp2) < (1 << 31)
}
