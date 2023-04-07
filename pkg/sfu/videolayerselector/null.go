package videolayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
)

type Null struct {
	*Base
}

func NewNull(logger logger.Logger) *Null {
	return &Null{
		Base: NewBase(logger),
	}
}

func (n *Null) IsOvershootOkay() bool {
	return false
}

func (n *Null) Select(_extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	return
}
