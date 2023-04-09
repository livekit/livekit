package temporallayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
)

type VP8 struct {
	logger logger.Logger
}

func NewVP8(logger logger.Logger) *VP8 {
	return &VP8{
		logger: logger,
	}
}

func (v *VP8) Select(extPkt *buffer.ExtPacket, current int32, target int32) (this int32, next int32) {
	this = current
	next = current
	if current == target {
		return
	}

	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok || !vp8.T {
		return
	}

	tid := int32(vp8.TID)
	if current < target {
		if tid > current && tid <= target && vp8.S && vp8.Y {
			this = tid
			next = tid
		}
	} else {
		if extPkt.Packet.Marker {
			next = target
		}
	}
	return
}
