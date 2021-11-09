package twcc

import (
	"encoding/binary"
	"math"
	"math/rand"
	"sort"
	"sync"

	"github.com/gammazero/deque"
	"github.com/pion/rtcp"
)

const (
	baseSequenceNumberOffset = 8
	packetStatusCountOffset  = 10
	referenceTimeOffset      = 12

	tccReportDelta          = 1e8
	tccReportDeltaAfterMark = 50e6
)

type rtpExtInfo struct {
	ExtTSN    uint32
	Timestamp int64
}

// Responder will get the transport wide sequence number from rtp
// extension header, and reply with the rtcp feedback message
// according to:
// https://tools.ietf.org/html/draft-holmer-rmcat-transport-wide-cc-extensions-01
type Responder struct {
	sync.Mutex

	extInfo     []rtpExtInfo
	lastReport  int64
	cycles      uint32
	lastExtSN   uint32
	pktCtn      uint8
	lastSn      uint16
	lastExtInfo uint16
	mSSRC       uint32
	sSSRC       uint32

	len      uint16
	deltaLen uint16
	payload  [100]byte
	deltas   [200]byte
	chunk    uint16

	onFeedback func(packet rtcp.RawPacket)
}

func NewTransportWideCCResponder(ssrc uint32) *Responder {
	return &Responder{
		extInfo: make([]rtpExtInfo, 0, 101),
		sSSRC:   rand.Uint32(),
		mSSRC:   ssrc,
	}
}

// Push a sequence number read from rtp packet ext packet
func (t *Responder) Push(sn uint16, timeNS int64, marker bool) {
	t.Lock()
	defer t.Unlock()

	if sn < 0x0fff && (t.lastSn&0xffff) > 0xf000 {
		t.cycles += 1 << 16
	}
	t.extInfo = append(t.extInfo, rtpExtInfo{
		ExtTSN:    t.cycles | uint32(sn),
		Timestamp: timeNS / 1e3,
	})
	if t.lastReport == 0 {
		t.lastReport = timeNS
	}
	t.lastSn = sn
	delta := timeNS - t.lastReport
	if len(t.extInfo) > 20 && t.mSSRC != 0 &&
		(delta >= tccReportDelta || len(t.extInfo) > 100 || (marker && delta >= tccReportDeltaAfterMark)) {
		if pkt := t.buildTransportCCPacket(); pkt != nil {
			t.onFeedback(pkt)
		}
		t.lastReport = timeNS
	}
}

// OnFeedback sets the callback for the formed twcc feedback rtcp packet
func (t *Responder) OnFeedback(f func(p rtcp.RawPacket)) {
	t.onFeedback = f
}

func (t *Responder) buildTransportCCPacket() rtcp.RawPacket {
	if len(t.extInfo) == 0 {
		return nil
	}
	sort.Slice(t.extInfo, func(i, j int) bool {
		return t.extInfo[i].ExtTSN < t.extInfo[j].ExtTSN
	})
	tccPkts := make([]rtpExtInfo, 0, int(float64(len(t.extInfo))*1.2))
	for _, tccExtInfo := range t.extInfo {
		if tccExtInfo.ExtTSN < t.lastExtSN {
			continue
		}
		if t.lastExtSN != 0 {
			for j := t.lastExtSN + 1; j < tccExtInfo.ExtTSN; j++ {
				tccPkts = append(tccPkts, rtpExtInfo{ExtTSN: j})
			}
		}
		t.lastExtSN = tccExtInfo.ExtTSN
		tccPkts = append(tccPkts, tccExtInfo)
	}
	t.extInfo = t.extInfo[:0]

	firstRecv := false
	same := true
	timestamp := int64(0)
	lastStatus := rtcp.TypeTCCPacketReceivedWithoutDelta
	maxStatus := rtcp.TypeTCCPacketNotReceived

	var statusList deque.Deque
	statusList.SetMinCapacity(3)

	for _, stat := range tccPkts {
		status := rtcp.TypeTCCPacketNotReceived
		if stat.Timestamp != 0 {
			var delta int64
			if !firstRecv {
				firstRecv = true
				refTime := stat.Timestamp / 64e3
				timestamp = refTime * 64e3
				t.writeHeader(
					uint16(tccPkts[0].ExtTSN),
					uint16(len(tccPkts)),
					uint32(refTime),
				)
				t.pktCtn++
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
				t.writeDelta(status, uint16(rDelta))
			} else {
				status = rtcp.TypeTCCPacketReceivedSmallDelta
				t.writeDelta(status, uint16(delta))
			}
			timestamp = stat.Timestamp
		}

		if same && status != lastStatus && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta {
			if statusList.Len() > 7 {
				t.writeRunLengthChunk(lastStatus, uint16(statusList.Len()))
				statusList.Clear()
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				same = true
			} else {
				same = false
			}
		}
		statusList.PushBack(status)
		if status > maxStatus {
			maxStatus = status
		}
		lastStatus = status

		if !same && maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta && statusList.Len() > 6 {
			for i := 0; i < 7; i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit, statusList.PopFront().(uint16), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit)
			lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
			maxStatus = rtcp.TypeTCCPacketNotReceived
			same = true

			for i := 0; i < statusList.Len(); i++ {
				status = statusList.At(i).(uint16)
				if status > maxStatus {
					maxStatus = status
				}
				if same && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta && status != lastStatus {
					same = false
				}
				lastStatus = status
			}
		} else if !same && statusList.Len() > 13 {
			for i := 0; i < 14; i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit, statusList.PopFront().(uint16), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit)
			lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
			maxStatus = rtcp.TypeTCCPacketNotReceived
			same = true
		}
	}

	if statusList.Len() > 0 {
		if same {
			t.writeRunLengthChunk(lastStatus, uint16(statusList.Len()))
		} else if maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta {
			for i := 0; i < statusList.Len(); i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit, statusList.PopFront().(uint16), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit)
		} else {
			for i := 0; i < statusList.Len(); i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit, statusList.PopFront().(uint16), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit)
		}
	}

	pLen := t.len + t.deltaLen + 4
	pad := pLen%4 != 0
	var padSize uint8
	for pLen%4 != 0 {
		padSize++
		pLen++
	}
	hdr := rtcp.Header{
		Padding: pad,
		Length:  (pLen / 4) - 1,
		Count:   rtcp.FormatTCC,
		Type:    rtcp.TypeTransportSpecificFeedback,
	}
	hb, _ := hdr.Marshal()
	pkt := make(rtcp.RawPacket, pLen)
	copy(pkt, hb)
	copy(pkt[4:], t.payload[:t.len])
	copy(pkt[4+t.len:], t.deltas[:t.deltaLen])
	if pad {
		pkt[len(pkt)-1] = padSize
	}
	t.deltaLen = 0
	return pkt
}

func (t *Responder) writeHeader(bSN, packetCount uint16, refTime uint32) {
	/*
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                     SSRC of packet sender                     |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                      SSRC of media source                     |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |      base sequence number     |      packet status count      |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                 reference time                | fb pkt. count |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	*/
	binary.BigEndian.PutUint32(t.payload[0:], t.sSSRC)
	binary.BigEndian.PutUint32(t.payload[4:], t.mSSRC)
	binary.BigEndian.PutUint16(t.payload[baseSequenceNumberOffset:], bSN)
	binary.BigEndian.PutUint16(t.payload[packetStatusCountOffset:], packetCount)
	binary.BigEndian.PutUint32(t.payload[referenceTimeOffset:], refTime<<8|uint32(t.pktCtn))
	t.len = 16
}

func (t *Responder) writeRunLengthChunk(symbol uint16, runLength uint16) {
	/*
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |T| S |       Run Length        |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	*/
	binary.BigEndian.PutUint16(t.payload[t.len:], symbol<<13|runLength)
	t.len += 2
}

func (t *Responder) createStatusSymbolChunk(symbolSize, symbol uint16, i int) {
	/*
		+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		|T|S|       symbol list         |
		+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	*/
	numOfBits := symbolSize + 1
	t.chunk = setNBitsOfUint16(t.chunk, numOfBits, numOfBits*uint16(i)+2, symbol)
}

func (t *Responder) writeStatusSymbolChunk(symbolSize uint16) {
	t.chunk = setNBitsOfUint16(t.chunk, 1, 0, 1)
	t.chunk = setNBitsOfUint16(t.chunk, 1, 1, symbolSize)
	binary.BigEndian.PutUint16(t.payload[t.len:], t.chunk)
	t.chunk = 0
	t.len += 2
}

func (t *Responder) writeDelta(deltaType, delta uint16) {
	if deltaType == rtcp.TypeTCCPacketReceivedSmallDelta {
		t.deltas[t.deltaLen] = byte(delta)
		t.deltaLen++
		return
	}
	binary.BigEndian.PutUint16(t.deltas[t.deltaLen:], delta)
	t.deltaLen += 2
}

// setNBitsOfUint16 will truncate the value to size, left-shift to startIndex position and set
func setNBitsOfUint16(src, size, startIndex, val uint16) uint16 {
	if startIndex+size > 16 {
		return 0
	}
	// truncate val to size bits
	val &= (1 << size) - 1
	return src | (val << (16 - size - startIndex))
}
