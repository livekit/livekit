package sendsidebwe

import (
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

// -------------------------------------------------------------------------------

var (
	errNoPacketInRange = errors.New("no packet in range")
)

// -------------------------------------------------------------------------------

type packetTrackerParams struct {
	Logger logger.Logger
}

type packetTracker struct {
	params packetTrackerParams

	lock sync.Mutex

	sequenceNumber uint64

	baseSendTime int64
	packetInfos  [2048]packetInfo

	baseRecvTime int64
	piLastRecv   *packetInfo

	inProbe                    bool
	probingStartSequenceNumber uint64
}

func NewPacketTracker(params packetTrackerParams) *packetTracker {
	return &packetTracker{
		params:         params,
		sequenceNumber: uint64(rand.Intn(1<<14)) + uint64(1<<15), // a random number in third quartile of sequence number space
	}
}

// SSBWE-TODO: this potentially needs to take isProbe as argument?
func (p *packetTracker) RecordPacketSendAndGetSequenceNumber(at time.Time, size int, isRTX bool) uint16 {
	p.lock.Lock()
	defer p.lock.Unlock()

	sendTime := at.UnixMicro()
	if p.baseSendTime == 0 {
		p.baseSendTime = sendTime
	}

	pi := p.getPacketInfo(uint16(p.sequenceNumber))
	pi.sequenceNumber = p.sequenceNumber
	pi.sendTime = sendTime - p.baseSendTime
	pi.recvTime = 0
	pi.size = uint16(size)
	pi.isRTX = isRTX
	// SSBWE-REMOVE p.params.Logger.Infow("packet sent", "packetInfo", pi) // SSBWE-REMOVE

	if p.inProbe && p.probingStartSequenceNumber == 0 {
		p.probingStartSequenceNumber = p.sequenceNumber
	}

	p.sequenceNumber++

	// extreme case of wrap around before receiving any feedback
	if pi == p.piLastRecv {
		p.piLastRecv = nil
	}

	return uint16(pi.sequenceNumber)
}

func (p *packetTracker) RecordPacketIndicationFromRemote(sn uint16, recvTime int64) (piRecv packetInfo, sendDelta, recvDelta int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	pi := p.getPacketInfoExisting(sn)
	if pi == nil {
		return
	}

	if recvTime == 0 {
		// maybe lost OR already receied but reported lost in a later report
		piRecv = *pi
		return
	}

	if p.baseRecvTime == 0 {
		p.baseRecvTime = recvTime
		p.piLastRecv = pi
	}

	pi.recvTime = recvTime - p.baseRecvTime
	piRecv = *pi
	if p.piLastRecv != nil {
		sendDelta, recvDelta = pi.sendTime-p.piLastRecv.sendTime, pi.recvTime-p.piLastRecv.recvTime
	}
	p.piLastRecv = pi
	return
}

func (p *packetTracker) ProbingStart() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.inProbe = true
	p.probingStartSequenceNumber = 0
}

func (p *packetTracker) ProbingEnd() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.inProbe = false
}

func (p *packetTracker) ProbingStartSequenceNumber() (uint64, bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.probingStartSequenceNumber == 0 {
		return 0, false
	}

	return p.probingStartSequenceNumber, true
}

func (p *packetTracker) getPacketInfo(sn uint16) *packetInfo {
	return &p.packetInfos[int(sn)%len(p.packetInfos)]
}

func (p *packetTracker) getPacketInfoExisting(sn uint16) *packetInfo {
	pi := &p.packetInfos[int(sn)%len(p.packetInfos)]
	if uint16(pi.sequenceNumber) == sn {
		return pi
	}

	return nil
}
