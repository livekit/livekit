package sendsidebwe

import (
	"sync"
	"time"
)

type packetInfo struct {
	sendTime    time.Time
	headerSize  uint16
	payloadSize uint16
}

type PacketTracker struct {
	lock        sync.RWMutex
	initialized bool
	highestSN   uint16
	packetInfos [1 << 16]packetInfo
}

func NewPacketTracker() *PacketTracker {
	return &PacketTracker{}
}

func (p *PacketTracker) PacketSent(at time.Time, sn uint16, headerSize int, payloadSize int) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.initialized {
		p.highestSN = sn - 1
		p.initialized = true
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
}

// ------------------------------------------------
