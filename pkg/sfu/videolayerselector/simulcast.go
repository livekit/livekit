package videolayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
)

type Simulcast struct {
	*Base
}

func NewSimulcast(logger logger.Logger) *Simulcast {
	return &Simulcast{
		Base: NewBase(logger),
	}
}

func NewSimulcastFromNull(vls VideoLayerSelector) *Simulcast {
	return &Simulcast{
		Base: vls.(*Null).Base,
	}
}

func (s *Simulcast) IsOvershootOkay() bool {
	return true
}

func (s *Simulcast) Select(extPkt *buffer.ExtPacket, layer int32) (result VideoLayerSelectorResult) {
	if s.currentLayer.Spatial != s.targetLayer.Spatial {
		// Three things to check when not locked to target
		//   1. Resumable layer - don't need a key frame
		//   2. Opportunistic layer upgrade - needs a key frame
		//   3. Need to downgrade - needs a key frame
		isActive := s.currentLayer.IsValid()
		found := false
		if s.parkedLayer.IsValid() {
			if s.parkedLayer.Spatial == layer {
				s.logger.Infow(
					"resuming at parked layer",
					"current", s.currentLayer,
					"target", s.targetLayer,
					"max", s.maxLayer,
					"parked", s.parkedLayer,
					"req", s.requestSpatial,
					"maxSeen", s.maxSeenLayer,
					"feed", extPkt.Packet.SSRC,
				)
				s.currentLayer = s.parkedLayer
				found = true
			}
		} else {
			if extPkt.KeyFrame {
				if layer > s.currentLayer.Spatial && layer <= s.targetLayer.Spatial {
					s.logger.Infow(
						"upgrading layer",
						"current", s.currentLayer,
						"target", s.targetLayer,
						"max", s.maxLayer,
						"layer", layer,
						"req", s.requestSpatial,
						"maxSeen", s.maxSeenLayer,
						"feed", extPkt.Packet.SSRC,
					)
					found = true
				}

				if layer < s.currentLayer.Spatial && layer >= s.targetLayer.Spatial {
					s.logger.Infow(
						"downgrading layer",
						"current", s.currentLayer,
						"target", s.targetLayer,
						"max", s.maxLayer,
						"layer", layer,
						"req", s.requestSpatial,
						"maxSeen", s.maxSeenLayer,
						"feed", extPkt.Packet.SSRC,
					)
					found = true
				}

				if found {
					s.currentLayer.Spatial = layer
					s.currentLayer.Temporal = extPkt.VideoLayer.Temporal
				}
			}
		}

		if found {
			if !isActive {
				result.IsResuming = true
			}
			s.SetParked(buffer.InvalidLayer)
			if s.currentLayer.Spatial >= s.maxLayer.Spatial {
				result.IsSwitchingToMaxSpatial = true

				s.logger.Infow(
					"reached max layer",
					"current", s.currentLayer,
					"target", s.targetLayer,
					"max", s.maxLayer,
					"layer", layer,
					"req", s.requestSpatial,
					"maxSeen", s.maxSeenLayer,
					"feed", extPkt.Packet.SSRC,
				)
			}

			if s.currentLayer.Spatial >= s.maxLayer.Spatial || s.currentLayer.Spatial == s.maxSeenLayer.Spatial {
				s.targetLayer.Spatial = s.currentLayer.Spatial
			}
		}
	}

	// if locked to higher than max layer due to overshoot, check if it can be dialed back
	if s.currentLayer.Spatial > s.maxLayer.Spatial {
		if layer <= s.maxLayer.Spatial && extPkt.KeyFrame {
			s.logger.Infow(
				"adjusting overshoot",
				"current", s.currentLayer,
				"target", s.targetLayer,
				"max", s.maxLayer,
				"layer", layer,
				"req", s.requestSpatial,
				"maxSeen", s.maxSeenLayer,
				"feed", extPkt.Packet.SSRC,
			)
			s.currentLayer.Spatial = layer

			if s.currentLayer.Spatial >= s.maxLayer.Spatial {
				result.IsSwitchingToMaxSpatial = true
			}

			if s.currentLayer.Spatial >= s.maxLayer.Spatial || s.currentLayer.Spatial == s.maxSeenLayer.Spatial {
				s.targetLayer.Spatial = layer
			}
		}
	}

	result.RTPMarker = extPkt.Packet.Marker
	result.IsSelected = layer == s.currentLayer.Spatial
	result.IsRelevant = false
	return
}
