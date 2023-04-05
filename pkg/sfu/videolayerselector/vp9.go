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

func (v *VP9) Select(extPkt *buffer.ExtPacket) (result VideoLayerSelectorResult) {
	vp9, ok := extPkt.Payload.(buffer.VP9)
	if !ok {
		return
	}

	isSwitchingLayer := false
	currentLayer := v.currentLayer
	updatedLayer := v.currentLayer
	if v.currentLayer != v.targetLayer {
		// temporal scale up/down
		if v.currentLayer.Temporal < v.targetLayer.Temporal {
			if extPkt.VideoLayer.Temporal > v.currentLayer.Temporal && extPkt.VideoLayer.Temporal <= v.targetLayer.Temporal && vp9.IsTemporalLayerSwitchUpPoint && vp9.IsBeginningOfFrame {
				isSwitchingLayer = true
				currentLayer.Temporal = extPkt.VideoLayer.Temporal
				updatedLayer.Temporal = extPkt.VideoLayer.Temporal
				v.logger.Infow("RAJA temporal layer switch up", "currentLayer", v.currentLayer, "cl", currentLayer, "targetLayer", v.targetLayer, "pid", vp9.PictureID) // REMOVE
			}
		} else {
			if extPkt.VideoLayer.Temporal < v.currentLayer.Temporal && extPkt.VideoLayer.Temporal >= v.targetLayer.Temporal && vp9.IsEndOfFrame {
				isSwitchingLayer = true
				updatedLayer.Temporal = extPkt.VideoLayer.Temporal
				v.logger.Infow("RAJA temporal layer switch down", "currentLayer", v.currentLayer, "cl", currentLayer, "targetLayer", v.targetLayer, "pid", vp9.PictureID) // REMOVE
			}
		}

		// spatial scale up/down
		if v.currentLayer.Spatial < v.targetLayer.Spatial {
			if extPkt.VideoLayer.Spatial > v.currentLayer.Spatial && extPkt.VideoLayer.Spatial <= v.targetLayer.Spatial && vp9.IsSpatialLayerSwitchUpPoint && vp9.IsBeginningOfFrame {
				isSwitchingLayer = true
				currentLayer.Spatial = extPkt.VideoLayer.Spatial
				updatedLayer.Spatial = extPkt.VideoLayer.Spatial
				v.logger.Infow("RAJA spatial layer switch up", "currentLayer", v.currentLayer, "cl", currentLayer, "targetLayer", v.targetLayer, "pid", vp9.PictureID) // REMOVE
			}
		} else {
			if extPkt.VideoLayer.Spatial < v.currentLayer.Spatial && extPkt.VideoLayer.Spatial >= v.targetLayer.Spatial && vp9.IsEndOfFrame {
				isSwitchingLayer = true
				updatedLayer.Spatial = extPkt.VideoLayer.Spatial
				v.logger.Infow("RAJA spatial layer switch down", "currentLayer", v.currentLayer, "cl", currentLayer, "targetLayer", v.targetLayer, "pid", vp9.PictureID) // REMOVE
			}
		}

		if updatedLayer.IsValid() {
			v.currentLayer = updatedLayer
			result.IsSwitchingLayer = isSwitchingLayer
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

/* RAJA-TODO
// at this point, either
// 1. dependency description has selected the layer for forwarding OR
// 2. non-dependency deescriptor is yet to make decision, but it can potentially switch to the incoming layer and start forwarding
//
// both cases cases upgrade/downgrade to current layer under the right conditions
if f.currentLayers.Spatial != f.targetLayers.Spatial {
	// Three things to check when not locked to target
	//   1. Resumable layer - don't need a key frame
	//   2. Opportunistic layer upgrade - needs a key frame if not using depedency descriptor
	//   3. Need to downgrade - needs a key frame if not using dependency descriptor
	found := false
	if f.parkedLayers.IsValid() {
		if f.parkedLayers.Spatial == layer {
			f.logger.Infow(
				"resuming at parked layer",
				"current", f.currentLayers,
				"target", f.targetLayers,
				"parked", f.parkedLayers,
				"feed", extPkt.Packet.SSRC,
			)
			f.currentLayers = f.parkedLayers
			found = true
		}
	} else {
		if extPkt.KeyFrame || tp.isSwitchingToTargetLayer {
			if layer > f.currentLayers.Spatial && layer <= f.targetLayers.Spatial {
				f.logger.Infow(
					"upgrading layer",
					"current", f.currentLayers,
					"target", f.targetLayers,
					"max", f.maxLayers,
					"layer", layer,
					"req", f.requestLayerSpatial,
					"maxPublished", f.maxPublishedLayer,
					"feed", extPkt.Packet.SSRC,
				)
				found = true
			}

			if layer < f.currentLayers.Spatial && layer >= f.targetLayers.Spatial {
				f.logger.Infow(
					"downgrading layer",
					"current", f.currentLayers,
					"target", f.targetLayers,
					"max", f.maxLayers,
					"layer", layer,
					"req", f.requestLayerSpatial,
					"maxPublished", f.maxPublishedLayer,
					"feed", extPkt.Packet.SSRC,
				)
				found = true
			}

			if found {
				f.currentLayers.Spatial = layer
				if !f.isTemporalSupported {
					f.currentLayers.Temporal = f.targetLayers.Temporal
				}
			}
		}
	}

	if found {
		tp.isSwitchingToTargetLayer = true
		f.clearParkedLayers()
		if f.currentLayers.Spatial >= f.maxLayers.Spatial {
			tp.isSwitchingToMaxLayer = true

			f.logger.Infow(
				"reached max layer",
				"current", f.currentLayers,
				"target", f.targetLayers,
				"max", f.maxLayers,
				"layer", layer,
				"req", f.requestLayerSpatial,
				"maxPublished", f.maxPublishedLayer,
				"feed", extPkt.Packet.SSRC,
			)
		}

		if f.currentLayers.Spatial >= f.maxLayers.Spatial || f.currentLayers.Spatial == f.maxPublishedLayer {
			f.targetLayers.Spatial = f.currentLayers.Spatial
			if f.vp9LayerSelector != nil {
				f.vp9LayerSelector.SelectLayer(f.targetLayers)
			}
			if f.ddLayerSelector != nil {
				f.ddLayerSelector.SelectLayer(f.targetLayers)
			}
		}
	}
}

// if locked to higher than max layer due to overshoot, check if it can be dialed back
if f.currentLayers.Spatial > f.maxLayers.Spatial {
	if layer <= f.maxLayers.Spatial && (extPkt.KeyFrame || tp.isSwitchingToTargetLayer) {
		f.logger.Infow(
			"adjusting overshoot",
			"current", f.currentLayers,
			"target", f.targetLayers,
			"max", f.maxLayers,
			"layer", layer,
			"req", f.requestLayerSpatial,
			"maxPublished", f.maxPublishedLayer,
			"feed", extPkt.Packet.SSRC,
		)
		f.currentLayers.Spatial = layer

		if f.currentLayers.Spatial >= f.maxLayers.Spatial {
			tp.isSwitchingToMaxLayer = true
		}

		if f.currentLayers.Spatial >= f.maxLayers.Spatial || f.currentLayers.Spatial == f.maxPublishedLayer {
			f.targetLayers.Spatial = layer
			if f.vp9LayerSelector != nil {
				f.vp9LayerSelector.SelectLayer(f.targetLayers)
			}
			if f.ddLayerSelector != nil {
				f.ddLayerSelector.SelectLayer(f.targetLayers)
			}
		}
	}
}
*/
