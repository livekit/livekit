package sendsidebwe

import (
	"time"
)

type PacketTracker struct {
	baseTime time.Time
}

func NewPacketTracker() *PacketTracker {
	return &PacketTracker{
		baseTime: time.Now(),
	}
}

// ------------------------------------------------
