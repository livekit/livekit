package videolayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp/codecs"
)

type VP9 struct {
	*Base
}

func NewVP9(logger logger.Logger) *VP9 {
	return &VP9{
		Base: NewBase(logger),
	}
}

func NewVP9FromNull(vls VideoLayerSelector) *VP9 {
	return &VP9{
		Base: vls.(*Null).Base,
	}
}

func (v *VP9) IsOvershootOkay() bool {
	return false
}

func (v *VP9) Select(extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	vp9, ok := extPkt.Payload.(codecs.VP9Packet)
	if !ok {
		return
	}

	currentLayer := v.currentLayer
	if v.currentLayer != v.targetLayer {
		updatedLayer := v.currentLayer

		if !v.currentLayer.IsValid() {
			if !extPkt.KeyFrame {
				return
			}

			updatedLayer = extPkt.VideoLayer
			currentLayer = extPkt.VideoLayer
		} else {
			// temporal scale up/down
			if v.currentLayer.Temporal < v.targetLayer.Temporal {
				if extPkt.VideoLayer.Temporal > v.currentLayer.Temporal && extPkt.VideoLayer.Temporal <= v.targetLayer.Temporal && vp9.U && vp9.B {
					updatedLayer.Temporal = extPkt.VideoLayer.Temporal
					currentLayer.Temporal = extPkt.VideoLayer.Temporal
				}
			} else {
				if extPkt.VideoLayer.Temporal < v.currentLayer.Temporal && extPkt.VideoLayer.Temporal >= v.targetLayer.Temporal && vp9.E {
					updatedLayer.Temporal = extPkt.VideoLayer.Temporal
				}
			}

			// spatial scale up/down
			if v.currentLayer.Spatial < v.targetLayer.Spatial {
				if extPkt.VideoLayer.Spatial > v.currentLayer.Spatial && extPkt.VideoLayer.Spatial <= v.targetLayer.Spatial && !vp9.P && vp9.B {
					updatedLayer.Spatial = extPkt.VideoLayer.Spatial
					currentLayer.Spatial = extPkt.VideoLayer.Spatial
				}
			} else {
				if extPkt.VideoLayer.Spatial < v.currentLayer.Spatial && extPkt.VideoLayer.Spatial >= v.targetLayer.Spatial && vp9.E {
					updatedLayer.Spatial = extPkt.VideoLayer.Spatial
				}
			}
		}

		if updatedLayer != v.currentLayer {
			if !v.currentLayer.IsValid() && updatedLayer.IsValid() {
				result.IsResuming = true
			}

			if v.currentLayer.Spatial != v.maxLayer.Spatial && updatedLayer.Spatial == v.maxLayer.Spatial {
				result.IsSwitchingToMaxSpatial = true
				v.logger.Infow(
					"reached max layer",
					"current", v.currentLayer,
					"target", v.targetLayer,
					"max", v.maxLayer,
					"layer", extPkt.VideoLayer.Spatial,
					"req", v.requestSpatial,
					"maxSeen", v.maxSeenLayer,
					"feed", extPkt.Packet.SSRC,
				)
			}

			v.currentLayer = updatedLayer
		}
	}

	result.RTPMarker = extPkt.Packet.Marker
	if extPkt.VideoLayer.Spatial == v.currentLayer.Spatial && vp9.E {
		result.RTPMarker = true
	}
	result.IsSelected = !extPkt.VideoLayer.GreaterThan(currentLayer)
	result.IsRelevant = true
	return
}
