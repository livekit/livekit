package sendsidebwe

import (
	"errors"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

// -------------------------------------------------------------------------------

var (
	errNoPacketInRange = errors.New("no packet in range")
)

// -------------------------------------------------------------------------------

type PacketTrackerParams struct {
	Logger logger.Logger
}

type PacketTracker struct {
	params PacketTrackerParams

	lock sync.Mutex

	baseSendTime   int64
	highestSentESN uint64
	packetInfos    [2048]packetInfo

	baseRecvTime  int64
	highestRecvSN uint16
}

func NewPacketTracker(params PacketTrackerParams) *PacketTracker {
	return &PacketTracker{
		params: params,
	}
}

// SSBWE-TODO: this potentially needs to take isProbe as argument?
func (p *PacketTracker) PacketSent(esn uint64, at time.Time, headerSize int, payloadSize int, isRTX bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	sendTime := at.UnixMicro()
	if p.baseSendTime == 0 {
		p.baseSendTime = sendTime
		p.highestSentESN = esn - 1
	}

	// old packet - should not happens packets should be sent in order
	if esn < p.highestSentESN {
		sn := uint16(esn)
		pi := p.getPacketInfo(sn)
		pi.Reset(sn)
		return
	}

	// clear slots occupied by missing packets,
	// ideally this should never run as seequence numbers should be generated in order
	// and packets sent in order.
	for i := p.highestSentESN + 1; i != esn; i++ {
		sn := uint16(i)
		pi := p.getPacketInfo(sn)
		pi.Reset(sn)
	}

	sn := uint16(esn)
	pi := p.getPacketInfo(sn)
	pi.sn = sn
	pi.sendTime = sendTime - p.baseSendTime
	pi.headerSize = uint8(headerSize)
	pi.payloadSize = uint16(payloadSize)
	pi.isRTX = isRTX
	pi.ResetReceiveAndDeltas()
	p.params.Logger.Infow("packet sent", "packetInfo", pi) // REMOVE

	p.highestSentESN = esn
}

func (p *PacketTracker) RecordPacketReceivedByRemote(sn uint16, recvTime int64) (pi *packetInfo, piPrev *packetInfo) {
	p.lock.Lock()
	defer p.lock.Unlock()

	pi = p.getPacketInfoExisting(sn)
	if pi == nil {
		return
	}

	if p.baseRecvTime == 0 {
		p.baseRecvTime = recvTime
		p.highestRecvSN = sn
	}

	pi.recvTime = recvTime - p.baseRecvTime

	// skip out-of-order deliveries
	if (sn - p.highestRecvSN) < (1 << 15) {
		piPrev = p.getPacketInfoExisting(p.highestRecvSN)
		if piPrev != nil {
			pi.sendDelta = pi.sendTime - piPrev.sendTime
			pi.recvDelta = pi.recvTime - piPrev.recvTime
			pi.deltaOfDelta = pi.recvDelta - pi.sendDelta
			p.params.Logger.Infow("packet received", "packetInfo", pi, "prev", piPrev) // REMOVE
			/* SSBWE-TODO - just make this comment and don't squash, such small differences should be accommodated
			in congestion detection
			if pi.deltaOfDelta < 0 && pi.deltaOfDelta > -rtcp.TypeTCCDeltaScaleFactor {
				// TWCC feedback has a resolution of 250 us inter packet interval,
				// squash small send intervals getting coalesced on the receiver side.
				// SSBWE-TODO: figure out proper adjustment for measurement resolution, this squelching is not always correct
				pi.deltaOfDelta = 0
			}
			*/
		}
		p.highestRecvSN = sn
	}
	return
}

func (p *PacketTracker) getPacketInfo(sn uint16) *packetInfo {
	return &p.packetInfos[int(sn)%len(p.packetInfos)]
}

func (p *PacketTracker) getPacketInfoExisting(sn uint16) *packetInfo {
	pi := &p.packetInfos[int(sn)%len(p.packetInfos)]
	if pi.sn == sn {
		return pi
	}

	return nil
}
