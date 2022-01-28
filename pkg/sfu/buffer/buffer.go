package buffer

import (
	"encoding/binary"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gammazero/deque"
	"github.com/go-logr/logr"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

const (
	ReportDelta = 1e9
)

// Logger is an implementation of logr.Logger. If is not provided - will be turned off.
var Logger = logr.Discard()

type pendingPackets struct {
	arrivalTime int64
	packet      []byte
}

type ExtPacket struct {
	Head     bool
	Arrival  int64
	Packet   rtp.Packet
	Payload  interface{}
	KeyFrame bool
	// audio level for voice, l&0x80 == 0 means audio level not present
	AudioLevel uint8
	RawPacket  []byte
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
	pPackets   []pendingPackets
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

	minPacketProbe     int
	lastPacketRead     int
	bitrate            atomic.Value
	bitrateHelper      [4]int64
	lastSRNTPTime      uint64
	lastSRRTPTime      uint32
	lastSRRecv         int64 // Represents wall clock of the most recent sender report arrival
	baseSN             uint16
	lastRtcpPacketTime int64 // Time the last RTCP packet was received.
	lastRtcpSrTime     int64 // Time the last RTCP SR was received. Required for DLSR computation.
	lastTransit        uint32
	seqHdlr            SeqWrapHandler

	stats Stats

	latestTimestamp          uint32 // latest received RTP timestamp on packet
	latestTimestampTime      int64  // Time of the latest timestamp (in nanos since unix epoch)
	lastFractionLostToReport uint8  // Last fraction lost from subscribers, should report to publisher; Audio only

	// callbacks
	onClose      func()
	onAudioLevel func(level uint8, durationMs uint32)
	feedbackCB   func([]rtcp.Packet)
	feedbackTWCC func(sn uint16, timeNS int64, marker bool)

	// logger
	logger logr.Logger
}

type Stats struct {
	LastExpected uint32
	LastReceived uint32
	LostRate     float32
	PacketCount  uint32  // Number of packets received from this source.
	Jitter       float64 // An estimate of the statistical variance of the RTP data packet inter-arrival time.
	TotalBytes   uint64
}

// BufferOptions provides configuration options for the buffer
type Options struct {
	MaxBitRate uint64
}

// NewBuffer constructs a new Buffer
func NewBuffer(ssrc uint32, vp, ap *sync.Pool, logger logr.Logger) *Buffer {
	b := &Buffer{
		mediaSSRC: ssrc,
		videoPool: vp,
		audioPool: ap,
		logger:    logger,
	}
	b.bitrate.Store(make([]int64, len(b.bitrateHelper)))
	b.extPackets.SetMinCapacity(7)
	return b
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
				b.logger.V(1).Info("Setting feedback", "type", "webrtc.TypeRTCPFBGoogREMB")
				b.remb = true
			case webrtc.TypeRTCPFBTransportCC:
				b.logger.V(1).Info("Setting feedback", "type", webrtc.TypeRTCPFBTransportCC)
				b.twcc = true
			case webrtc.TypeRTCPFBNACK:
				b.logger.V(1).Info("Setting feedback", "type", webrtc.TypeRTCPFBNACK)
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

	b.logger.V(1).Info("NewBuffer", "MaxBitRate", o.MaxBitRate)
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
		b.pPackets = append(b.pPackets, pendingPackets{
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

	var headPkt bool
	if b.stats.PacketCount == 0 {
		b.baseSN = sn
		b.lastReport = arrivalTime
		b.seqHdlr.UpdateMaxSeq(uint32(sn))
		headPkt = true
	} else {
		extSN, isNewer := b.seqHdlr.Unwrap(sn)
		if b.nack {
			if isNewer {
				for i := b.seqHdlr.MaxSeqNo() + 1; i < extSN; i++ {
					b.nacker.Push(i)
				}
			} else {
				b.nacker.Remove(extSN)
			}
		}
		if isNewer {
			b.seqHdlr.UpdateMaxSeq(extSN)
		}
		headPkt = isNewer
	}

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

	if b.minPacketProbe < 25 {
		// LK-TODO-START
		// This should check for proper wrap around.
		// Probably remove this probe section of code as
		// the only place this baseSN is used at is where
		// RTCP receiver reports are generated. If there
		// are some out-of-order packets right at the start
		// the stat is going to be off by a bit. Not a big deal.
		// LK-TODO-END
		if sn < b.baseSN {
			b.baseSN = sn
		}

		b.minPacketProbe++
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
				ep.AudioLevel = e[0] | 0x80 // highest bit to indicate audio level present
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

	diff := arrivalTime - b.lastReport
	if diff >= ReportDelta {
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
			br := (8 * b.bitrateHelper[i] * int64(ReportDelta)) / diff
			bitrates[i] = br
			b.bitrateHelper[i] = 0
		}
		b.bitrate.Store(bitrates)
		b.feedbackCB(b.getRTCP())
		b.lastReport = arrivalTime
	}
}

func (b *Buffer) buildNACKPacket() []rtcp.Packet {
	if nacks, askKeyframe := b.nacker.Pairs(b.seqHdlr.MaxSeqNo()); len(nacks) > 0 || askKeyframe {
		var pkts []rtcp.Packet
		if len(nacks) > 0 {
			pkts = []rtcp.Packet{&rtcp.TransportLayerNack{
				MediaSSRC: b.mediaSSRC,
				Nacks:     nacks,
			}}
		}

		if askKeyframe {
			pkts = append(pkts, &rtcp.PictureLossIndication{
				MediaSSRC: b.mediaSSRC,
			})
		}
		return pkts
	}
	return nil
}

func (b *Buffer) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {
	br := b.Bitrate()
	if b.stats.LostRate < 0.02 {
		br = int64(float64(br)*1.09) + 2000
	}
	if b.stats.LostRate > .1 {
		br = int64(float64(br) * float64(1-0.5*b.stats.LostRate))
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

func (b *Buffer) buildReceptionReport() rtcp.ReceptionReport {
	extMaxSeq := b.seqHdlr.MaxSeqNo()
	expected := extMaxSeq - uint32(b.baseSN) + 1
	lost := uint32(0)
	if b.stats.PacketCount < expected && b.stats.PacketCount != 0 {
		lost = expected - b.stats.PacketCount
	}
	expectedInterval := expected - b.stats.LastExpected
	b.stats.LastExpected = expected

	receivedInterval := b.stats.PacketCount - b.stats.LastReceived
	b.stats.LastReceived = b.stats.PacketCount

	lostInterval := expectedInterval - receivedInterval

	var fracLost uint8
	if expectedInterval != 0 {
		b.stats.LostRate = float32(lostInterval) / float32(expectedInterval)
		fracLost = uint8((lostInterval << 8) / expectedInterval)
	}
	if b.lastFractionLostToReport > fracLost {
		// If fraction lost from subscriber is bigger than sfu received, use it.
		fracLost = b.lastFractionLostToReport
	}

	var dlsr uint32
	if b.lastSRRecv != 0 {
		delayMS := uint32((time.Now().UnixNano() - b.lastSRRecv) / 1e6)
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}

	rr := rtcp.ReceptionReport{
		SSRC:               b.mediaSSRC,
		FractionLost:       fracLost,
		TotalLost:          lost,
		LastSequenceNumber: extMaxSeq,
		Jitter:             uint32(b.stats.Jitter),
		LastSenderReport:   uint32(b.lastSRNTPTime >> 16),
		Delay:              dlsr,
	}
	return rr
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

	pkts = append(pkts, &rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{b.buildReceptionReport()},
	})

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

// GetLatestTimestamp returns the latest RTP timestamp factoring in potential RTP timestamp wrap-around
func (b *Buffer) GetLatestTimestamp() (latestTimestamp uint32, latestTimestampTimeInNanosSinceEpoch int64) {
	latestTimestamp = atomic.LoadUint32(&b.latestTimestamp)
	latestTimestampTimeInNanosSinceEpoch = atomic.LoadInt64(&b.latestTimestampTime)

	return latestTimestamp, latestTimestampTimeInNanosSinceEpoch
}

// IsTimestampWrapAround returns true if wrap around happens from timestamp1 to timestamp2
func IsTimestampWrapAround(timestamp1 uint32, timestamp2 uint32) bool {
	return timestamp2 < timestamp1 && timestamp1 > 0xf0000000 && timestamp2 < 0x0fffffff
}

// IsLaterTimestamp returns true if timestamp1 is later in time than timestamp2 factoring in timestamp wrap-around
func IsLaterTimestamp(timestamp1 uint32, timestamp2 uint32) bool {
	if timestamp1 > timestamp2 {
		if IsTimestampWrapAround(timestamp1, timestamp2) {
			return false
		}
		return true
	}
	if IsTimestampWrapAround(timestamp2, timestamp1) {
		return true
	}
	return false
}

func IsNewerUint16(val1, val2 uint16) bool {
	return val1 != val2 && val1-val2 < 0x8000
}

type SeqWrapHandler struct {
	maxSeqNo uint32
}

func (s *SeqWrapHandler) Cycles() uint32 {
	return s.maxSeqNo & 0xffff0000
}

func (s *SeqWrapHandler) MaxSeqNo() uint32 {
	return s.maxSeqNo
}

// unwrap seq and update the maxSeqNo. return unwrapped value, and whether seq is newer
func (s *SeqWrapHandler) Unwrap(seq uint16) (uint32, bool) {

	maxSeqNo := uint16(s.maxSeqNo)
	delta := int32(seq) - int32(maxSeqNo)

	newer := IsNewerUint16(seq, maxSeqNo)

	if newer {
		if delta < 0 {
			// seq is newer, but less than maxSeqNo, wrap around
			delta += 0x10000
		}
	} else {
		// older value
		if delta > 0 && (int32(s.maxSeqNo)+delta-0x10000) >= 0 {
			// wrap backwards, should not less than 0 in this case:
			//   at start time, received seq 1, set s.maxSeqNo =1 ,
			//   then an out of order seq 65534 coming, we can't unwrap
			//   the seq to -2
			delta -= 0x10000
		}
	}

	unwrapped := uint32(int32(s.maxSeqNo) + delta)
	return unwrapped, newer
}

func (s *SeqWrapHandler) UpdateMaxSeq(extSeq uint32) {
	s.maxSeqNo = extSeq
}
