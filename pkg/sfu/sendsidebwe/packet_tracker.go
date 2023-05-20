package sendsidebwe

import (
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

type packetInfo struct {
	sendTime    time.Time
	arrivalTime int64
	headerSize  uint16
	payloadSize uint16
}

type PacketTracker struct {
	logger logger.Logger

	lock        sync.RWMutex
	initialized bool
	highestSN   uint16
	packetInfos [1 << 16]packetInfo
}

func NewPacketTracker(logger logger.Logger) *PacketTracker {
	return &PacketTracker{
		logger: logger,
	}
}

func (p *PacketTracker) PacketSent(sn uint16, at time.Time, headerSize int, payloadSize int) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.initialized {
		p.highestSN = sn - 1
		p.initialized = true
	}

	// should never happen, but just a sanity check
	if (sn - p.highestSN) > (1 << 15) {
		return
	}

	// clear slots occupied by missing packets,
	// ideally this should never run as seequence numbers should be generated in order
	// and packets sent in order.
	for i := p.highestSN + 1; i != sn; i++ {
		pi := &p.packetInfos[i]
		pi.sendTime = time.Time{}
		pi.headerSize = 0
		pi.payloadSize = 0
	}

	pi := &p.packetInfos[sn]
	pi.sendTime = at
	pi.headerSize = uint16(headerSize)
	pi.payloadSize = uint16(payloadSize)

	p.highestSN = sn
}

func (p *PacketTracker) ProcessFeedback(baseSN uint16, arrivals []int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	for i, arrival := range arrivals {
		p.packetInfos[baseSN+uint16(i)].arrivalTime = arrival
	}
}

// ------------------------------------------------
