package temporallayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

type Null struct{}

func NewNull() *Null {
	return &Null{}
}

func Select(_extPkt *buffer.ExtPacket, current int32, _target int32) (this int32, next int32) {
	this = current
	next = current
	return
}
