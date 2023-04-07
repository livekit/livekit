package videolayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
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
	vp9, ok := extPkt.Payload.(buffer.VP9)
	if !ok {
		return
	}

	isActive := v.currentLayer.IsValid()
	isResuming := false
	currentLayer := v.currentLayer
	updatedLayer := v.currentLayer
	if v.currentLayer != v.targetLayer {
		// temporal scale up/down
		if v.currentLayer.Temporal < v.targetLayer.Temporal {
			if extPkt.VideoLayer.Temporal > v.currentLayer.Temporal && extPkt.VideoLayer.Temporal <= v.targetLayer.Temporal && vp9.IsTemporalLayerSwitchUpPoint && vp9.IsBeginningOfFrame {
				if !isActive {
					isResuming = true
				}
				currentLayer.Temporal = extPkt.VideoLayer.Temporal
				updatedLayer.Temporal = extPkt.VideoLayer.Temporal
				v.logger.Infow("RAJA temporal layer switch up", "currentLayer", v.currentLayer, "cl", currentLayer, "targetLayer", v.targetLayer, "pid", vp9.PictureID) // REMOVE
			}
		} else {
			if extPkt.VideoLayer.Temporal < v.currentLayer.Temporal && extPkt.VideoLayer.Temporal >= v.targetLayer.Temporal && vp9.IsEndOfFrame {
				if !isActive {
					isResuming = true
				}
				updatedLayer.Temporal = extPkt.VideoLayer.Temporal
				v.logger.Infow("RAJA temporal layer switch down", "currentLayer", v.currentLayer, "cl", currentLayer, "targetLayer", v.targetLayer, "pid", vp9.PictureID) // REMOVE
			}
		}

		// spatial scale up/down
		if v.currentLayer.Spatial < v.targetLayer.Spatial {
			if extPkt.VideoLayer.Spatial > v.currentLayer.Spatial && extPkt.VideoLayer.Spatial <= v.targetLayer.Spatial && vp9.IsSpatialLayerSwitchUpPoint && vp9.IsBeginningOfFrame {
				if !isActive {
					isResuming = true
				}
				currentLayer.Spatial = extPkt.VideoLayer.Spatial
				updatedLayer.Spatial = extPkt.VideoLayer.Spatial
				v.logger.Infow("RAJA spatial layer switch up", "currentLayer", v.currentLayer, "cl", currentLayer, "targetLayer", v.targetLayer, "pid", vp9.PictureID) // REMOVE
			}
		} else {
			if extPkt.VideoLayer.Spatial < v.currentLayer.Spatial && extPkt.VideoLayer.Spatial >= v.targetLayer.Spatial && vp9.IsEndOfFrame {
				if !isActive {
					isResuming = true
				}
				updatedLayer.Spatial = extPkt.VideoLayer.Spatial
				v.logger.Infow("RAJA spatial layer switch down", "currentLayer", v.currentLayer, "cl", currentLayer, "targetLayer", v.targetLayer, "pid", vp9.PictureID) // REMOVE
			}
		}

		if updatedLayer.IsValid() {
			v.currentLayer = updatedLayer
			result.IsResuming = isResuming
		}
	}

	result.RTPMarker = extPkt.Packet.Marker
	if extPkt.VideoLayer.Spatial == v.currentLayer.Spatial && vp9.IsEndOfFrame {
		result.RTPMarker = true
	}
	result.IsSelected = !extPkt.VideoLayer.GreaterThan(currentLayer)
	result.IsRelevant = true
	return
}
