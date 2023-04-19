package codecmunger

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
)

type Null struct {
	seededState interface{}
}

func NewNull(_logger logger.Logger) *Null {
	return &Null{}
}

func (n *Null) GetState() interface{} {
	return nil
}

func (n *Null) SeedState(state interface{}) {
	n.seededState = state
}

func (n *Null) GetSeededState() interface{} {
	return n.seededState
}

func (n *Null) SetLast(_extPkt *buffer.ExtPacket) {
}

func (n *Null) UpdateOffsets(_extPkt *buffer.ExtPacket) {
}

func (n *Null) UpdateAndGet(_extPkt *buffer.ExtPacket, snOutOfOrder bool, snHasGap bool, maxTemporal int32) ([]byte, error) {
	return nil, nil
}

func (n *Null) UpdateAndGetPadding(newPicture bool) ([]byte, error) {
	return nil, nil
}
