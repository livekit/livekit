package pacer

import (
	"github.com/livekit/protocol/logger"
)

type PassThrough struct {
	*Base
}

func NewPassThrough(logger logger.Logger) *PassThrough {
	return &PassThrough{
		Base: NewBase(logger),
	}
}

func (p *PassThrough) Stop() {
}

func (p *PassThrough) Enqueue(pkt Packet) {
	p.Base.SendPacket(&pkt)
}

// ------------------------------------------------
