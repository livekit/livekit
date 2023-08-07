package sendsidebwe

import (
	"errors"
	"time"
)

// -------------------------------------------------------------

var (
	errOutOfRange = errors.New("packet out of range")
)

// -------------------------------------------------------------

type PacketGroup struct {
	spread time.Duration

	packetInfos []*packetInfo
	minSendTime time.Time
}

func NewPacketGroup(spread time.Duration) *PacketGroup {
	return &PacketGroup{
		spread: spread,
	}
}

func (p *PacketGroup) Add(pi *packetInfo) error {
	if !p.minSendTime.IsZero() && pi.sendTime.Sub(p.minSendTime) > p.spread {
		return errOutOfRange
	}

	p.packetInfos = append(p.packetInfos, pi)
	if p.minSendTime.IsZero() || pi.sendTime.Before(p.minSendTime) {
		p.minSendTime = pi.sendTime
	}
	return nil
}

/* RAJA-REMOVE
func (p *PacketGroup) GetMinSendTime() time.Time {
	minSendTime := time.Time{}
	for _, pi := range p.packetInfos {
		if minSendTime.IsZero() || pi.sendTime.Before(minSendTime) {
			minSendTime = pi.sendTime
		}
	}

	return minSendTime
}

func (p *PacketGroup) GetMaxSendTime() time.Time {
	maxSendTime := time.Time{}
	for _, pi := range p.packetInfos {
		if maxSendTime.IsZero() || pi.sendTime.After(maxSendTime) {
			maxSendTime = pi.sendTime
		}
	}

	return maxSendTime
}
*/
