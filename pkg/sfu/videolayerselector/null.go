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

func (n *Null) Select(extPkt *buffer.ExtPacket) (result VideoLayerSelectorResult) {
	return
}
