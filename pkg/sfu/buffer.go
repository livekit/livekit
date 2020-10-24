package sfu

// file from ion-sfu project

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

const (
	maxSN = 1 << 16
	// default buffer time by ms
	defaultBufferTime = 1000
)

type rtpExtInfo struct {
	ExtTSN    uint32
	Timestamp int64
}

// Buffer contains all packets
type Buffer struct {
	mu sync.RWMutex

	pktQueue   queue
	codecType  webrtc.RTPCodecType
	simulcast  bool
	clockRate  uint32
	maxBitrate uint64

	// supported feedbacks
	remb bool
	nack bool
	tcc  bool

	lastSRNTPTime      uint64
	lastSRRTPTime      uint32
	lastSRRecv         int64 // Represents wall clock of the most recent sender report arrival
	baseSN             uint16
	cycles             uint32
	lastExpected       uint32
	lastReceived       uint32
	lostRate           float32
	ssrc               uint32
	lastPacketTime     int64  // Time the last RTP packet from this source was received
	lastRtcpPacketTime int64  // Time the last RTCP packet was received.
	lastRtcpSrTime     int64  // Time the last RTCP SR was received. Required for DLSR computation.
	packetCount        uint32 // Number of packets received from this source.
	lastTransit        uint32
	maxSeqNo           uint16  // The highest sequence number received in an RTP data packet
	jitter             float64 // An estimate of the statistical variance of the RTP data packet inter-arrival time.
	totalByte          uint64

	// remb
	rembSteps uint8

	// transport-cc
	tccExt       uint8
	tccExtInfo   []rtpExtInfo
	tccCycles    uint32
	tccLastExtSN uint32
	tccPktCtn    uint8
	tccLastSn    uint16
	lastExtInfo  uint16
}

// BufferOptions provides configuration options for the buffer
type BufferOptions struct {
	TCCExt     int
	BufferTime int
	MaxBitRate uint64
}

// NewBuffer constructs a new Buffer
func NewBuffer(track *webrtc.Track, o BufferOptions) *Buffer {
	b := &Buffer{
		ssrc:       track.SSRC(),
		clockRate:  track.Codec().ClockRate,
		codecType:  track.Codec().Type,
		maxBitrate: o.MaxBitRate,
		simulcast:  len(track.RID()) > 0,
		rembSteps:  4,
	}
	if o.BufferTime <= 0 {
		o.BufferTime = defaultBufferTime
	}
	b.pktQueue.duration = uint32(o.BufferTime) * b.clockRate / 1000
	b.pktQueue.ssrc = track.SSRC()
	b.tccExt = uint8(o.TCCExt)

	for _, fb := range track.Codec().RTCPFeedback {
		switch fb.Type {
		case webrtc.TypeRTCPFBGoogREMB:
			//log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBGoogREMB)
			b.remb = true
		case webrtc.TypeRTCPFBTransportCC:
			//log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBTransportCC)
			b.tccExtInfo = make([]rtpExtInfo, 1<<8)
			b.tcc = true
		case webrtc.TypeRTCPFBNACK:
			//log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBNACK)
			b.nack = true
		}
	}
	//log.Debugf("NewBuffer BufferOptions=%v", o)
	return b
}

// Push adds a RTP Packet, out of order, new packet may be arrived later
func (b *Buffer) Push(p *rtp.Packet) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.totalByte += uint64(p.MarshalSize())
	if b.packetCount == 0 {
		b.baseSN = p.SequenceNumber
		b.maxSeqNo = p.SequenceNumber
		b.pktQueue.headSN = p.SequenceNumber - 1
	} else if snDiff(b.maxSeqNo, p.SequenceNumber) <= 0 {
		if p.SequenceNumber < b.maxSeqNo {
			b.cycles += maxSN
		}
		b.maxSeqNo = p.SequenceNumber
	}
	b.packetCount++
	b.lastPacketTime = time.Now().UnixNano()
	arrival := uint32(b.lastPacketTime / 1e6 * int64(b.clockRate/1e3))
	transit := arrival - p.Timestamp
	if b.lastTransit != 0 {
		d := int32(transit - b.lastTransit)
		if d < 0 {
			d = -d
		}
		b.jitter += (float64(d) - b.jitter) / 16
	}
	b.lastTransit = transit
	if b.codecType == webrtc.RTPCodecTypeVideo {
		b.pktQueue.AddPacket(p, p.SequenceNumber == b.maxSeqNo)
	}

	if b.tcc {
		rtpTCC := rtp.TransportCCExtension{}
		if err := rtpTCC.Unmarshal(p.GetExtension(b.tccExt)); err == nil {
			if rtpTCC.TransportSequence < 0x0fff && (b.tccLastSn&0xffff) > 0xf000 {
				b.tccCycles += maxSN
			}
			b.tccExtInfo = append(b.tccExtInfo, rtpExtInfo{
				ExtTSN:    b.tccCycles | uint32(rtpTCC.TransportSequence),
				Timestamp: b.lastPacketTime / 1e3,
			})
		}
	}
}

func (b *Buffer) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {
	br := b.maxBitrate
	if b.rembSteps > 0 {
		br /= uint64(b.rembSteps)
		b.rembSteps--
	}

	return &rtcp.ReceiverEstimatedMaximumBitrate{
		SenderSSRC: b.ssrc,
		Bitrate:    br,
		SSRCs:      []uint32{b.ssrc},
	}
}

func (b *Buffer) buildTransportCCPacket() *rtcp.TransportLayerCC {
	if len(b.tccExtInfo) == 0 {
		return nil
	}
	sort.Slice(b.tccExtInfo, func(i, j int) bool {
		return b.tccExtInfo[i].ExtTSN < b.tccExtInfo[j].ExtTSN
	})
	tccPkts := make([]rtpExtInfo, 0, int(float64(len(b.tccExtInfo))*1.2))
	for _, tccExtInfo := range b.tccExtInfo {
		if tccExtInfo.ExtTSN < b.tccLastExtSN {
			continue
		}
		if b.tccLastExtSN != 0 {
			for j := b.tccLastExtSN + 1; j < tccExtInfo.ExtTSN; j++ {
				tccPkts = append(tccPkts, rtpExtInfo{ExtTSN: j})
			}
		}
		b.tccLastExtSN = tccExtInfo.ExtTSN
		tccPkts = append(tccPkts, tccExtInfo)
	}
	b.tccExtInfo = b.tccExtInfo[:0]

	rtcpTCC := &rtcp.TransportLayerCC{
		Header: rtcp.Header{
			Padding: true,
			Count:   rtcp.FormatTCC,
			Type:    rtcp.TypeTransportSpecificFeedback,
		},
		MediaSSRC:          b.ssrc,
		BaseSequenceNumber: uint16(tccPkts[0].ExtTSN),
		PacketStatusCount:  uint16(len(tccPkts)),
		FbPktCount:         b.tccPktCtn,
	}
	b.tccPktCtn++

	firstRecv := false
	allSame := true
	timestamp := int64(0)
	deltaLen := 0
	lastStatus := rtcp.TypeTCCPacketReceivedWithoutDelta
	maxStatus := rtcp.TypeTCCPacketNotReceived

	var statusList deque.Deque

	for _, stat := range tccPkts {
		status := rtcp.TypeTCCPacketNotReceived
		if stat.Timestamp != 0 {
			var delta int64
			if !firstRecv {
				firstRecv = true
				timestamp = stat.Timestamp
				rtcpTCC.ReferenceTime = uint32(stat.Timestamp / 64000)
			}

			delta = (stat.Timestamp - timestamp) / 250
			if delta < 0 || delta > 255 {
				status = rtcp.TypeTCCPacketReceivedLargeDelta
				rDelta := int16(delta)
				if int64(rDelta) != delta {
					if rDelta > 0 {
						rDelta = math.MaxInt16
					} else {
						rDelta = math.MinInt16
					}
				}
				rtcpTCC.RecvDeltas = append(rtcpTCC.RecvDeltas, &rtcp.RecvDelta{
					Type:  status,
					Delta: int64(rDelta) * 250,
				})
				deltaLen += 2
			} else {
				status = rtcp.TypeTCCPacketReceivedSmallDelta
				rtcpTCC.RecvDeltas = append(rtcpTCC.RecvDeltas, &rtcp.RecvDelta{
					Type:  status,
					Delta: delta * 250,
				})
				deltaLen++
			}
			timestamp = stat.Timestamp
		}

		if allSame && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta && status != lastStatus {
			if statusList.Len() > 7 {
				rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.RunLengthChunk{
					PacketStatusSymbol: lastStatus,
					RunLength:          uint16(statusList.Len()),
				})
				statusList.Clear()
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				allSame = true
			} else {
				allSame = false
			}
		}
		statusList.PushBack(status)
		if status > maxStatus {
			maxStatus = status
		}
		lastStatus = status

		if !allSame {
			if maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta && statusList.Len() > 6 {
				symbolList := make([]uint16, 7)
				for i := 0; i < 7; i++ {
					symbolList[i] = statusList.PopFront().(uint16)
				}
				rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.StatusVectorChunk{
					SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
					SymbolList: symbolList,
				})
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				allSame = true

				for i := 0; i < statusList.Len(); i++ {
					status = statusList.At(i).(uint16)
					if status > maxStatus {
						maxStatus = status
					}
					if allSame && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta && status != lastStatus {
						allSame = false
					}
					lastStatus = status
				}
			} else if statusList.Len() > 13 {
				symbolList := make([]uint16, 14)
				for i := 0; i < 14; i++ {
					symbolList[i] = statusList.PopFront().(uint16)
				}
				rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.StatusVectorChunk{
					SymbolSize: rtcp.TypeTCCSymbolSizeOneBit,
					SymbolList: symbolList,
				})
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				allSame = true
			}
		}
	}

	if statusList.Len() > 0 {
		if allSame {
			rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.RunLengthChunk{
				PacketStatusSymbol: lastStatus,
				RunLength:          uint16(statusList.Len()),
			})
		} else if maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta {
			symbolList := make([]uint16, statusList.Len())
			for i := 0; i < statusList.Len(); i++ {
				symbolList[i] = statusList.PopFront().(uint16)
			}
			rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.StatusVectorChunk{
				SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
				SymbolList: symbolList,
			})
		} else {
			symbolList := make([]uint16, statusList.Len())
			for i := 0; i < statusList.Len(); i++ {
				symbolList[i] = statusList.PopFront().(uint16)
			}
			rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.StatusVectorChunk{
				SymbolSize: rtcp.TypeTCCSymbolSizeOneBit,
				SymbolList: symbolList,
			})
		}
	}

	pLen := uint16(20 + len(rtcpTCC.PacketChunks)*2 + deltaLen)
	rtcpTCC.Header.Padding = pLen%4 != 0
	for pLen%4 != 0 {
		pLen++
	}
	rtcpTCC.Header.Length = (pLen / 4) - 1
	return rtcpTCC
}

func (b *Buffer) buildReceptionReport() rtcp.ReceptionReport {
	extMaxSeq := b.cycles | uint32(b.maxSeqNo)
	expected := extMaxSeq - uint32(b.baseSN) + 1
	lost := expected - b.packetCount
	if b.packetCount == 0 {
		lost = 0
	}
	expectedInterval := expected - b.lastExpected
	b.lastExpected = expected

	receivedInterval := b.packetCount - b.lastReceived
	b.lastReceived = b.packetCount

	lostInterval := expectedInterval - receivedInterval

	b.lostRate = float32(lostInterval) / float32(expectedInterval)
	var fracLost uint8
	if expectedInterval != 0 && lostInterval > 0 {
		fracLost = uint8((lostInterval << 8) / expectedInterval)
	}
	var dlsr uint32
	if b.lastSRRecv != 0 {
		delayMS := uint32((time.Now().UnixNano() - b.lastSRRecv) / 1e6)
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}

	rr := rtcp.ReceptionReport{
		SSRC:               b.ssrc,
		FractionLost:       fracLost,
		TotalLost:          lost,
		LastSequenceNumber: extMaxSeq,
		Jitter:             uint32(b.jitter),
		LastSenderReport:   uint32(b.lastSRNTPTime >> 16),
		Delay:              dlsr,
	}
	return rr
}

func (b *Buffer) SetSenderReportData(rtpTime uint32, ntpTime uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastSRRTPTime = rtpTime
	b.lastSRNTPTime = ntpTime
	b.lastSRRecv = time.Now().UnixNano()
}

func (b *Buffer) BuildRTCP() (rtcp.ReceptionReport, []rtcp.Packet) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var pkts []rtcp.Packet
	var report rtcp.ReceptionReport

	report = b.buildReceptionReport()

	if b.remb {
		pkts = append(pkts, b.buildREMBPacket())
	}

	if b.tcc {
		if tccPkt := b.buildTransportCCPacket(); tccPkt != nil {
			pkts = append(pkts, tccPkt)
		}
	}

	return report, pkts
}

// WritePacket write buffer packet to requested track. and modify headers
func (b *Buffer) WritePacket(sn uint16, track *webrtc.Track, snOffset uint16, tsOffset, ssrc uint32) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if bufferPkt := b.pktQueue.GetPacket(sn); bufferPkt != nil {
		bSsrc := bufferPkt.SSRC
		bufferPkt.SequenceNumber -= snOffset
		bufferPkt.Timestamp -= tsOffset
		bufferPkt.SSRC = ssrc
		err := track.WriteRTP(bufferPkt)
		bufferPkt.Timestamp += tsOffset
		bufferPkt.SequenceNumber += snOffset
		bufferPkt.SSRC = bSsrc
		return err
	}
	return errPacketNotFound
}

func (b *Buffer) onLostHandler(fn func(nack *rtcp.TransportLayerNack)) {
	if b.nack {
		b.pktQueue.onLost = fn
	}
}
