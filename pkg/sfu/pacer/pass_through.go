package pacer

import (
	"github.com/livekit/livekit-server/pkg/sfu/sendsidebwe"
	"github.com/livekit/protocol/logger"
)

type PassThrough struct {
	*Base
}

func NewPassThrough(logger logger.Logger, sendSideBWE *sendsidebwe.SendSideBWE) *PassThrough {
	return &PassThrough{
		Base: NewBase(logger, sendSideBWE),
	}
}

func (p *PassThrough) Stop() {
}

func (p *PassThrough) Enqueue(pkt Packet) {
	p.Base.SendPacket(&pkt)
}

// ------------------------------------------------
