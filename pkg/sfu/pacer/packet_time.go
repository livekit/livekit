package pacer

import (
	"time"
)

type PacketTime struct {
	baseTime time.Time
}

func NewPacketTime() *PacketTime {
	return &PacketTime{
		baseTime: time.Now(),
	}
}

func (p *PacketTime) Get() time.Time {
	// construct current time based on monotonic clock
	return p.baseTime.Add(time.Since(p.baseTime))
}

// ------------------------------------------------
