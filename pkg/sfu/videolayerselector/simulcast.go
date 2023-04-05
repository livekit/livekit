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

func (s *Simulcast) Select(extPkt *buffer.ExtPacket) (result VideoLayerSelectorResult) {
	// RAJA-TODO: drop up front if target is invalid
	if s.currentLayer.Spatial != s.targetLayer.Spatial {
		// Three things to check when not locked to target
		//   1. Resumable layer - don't need a key frame
		//   2. Opportunistic layer upgrade - needs a key frame
		//   3. Need to downgrade - needs a key frame
		found := false
		if s.parkedLayer.IsValid() {
			if s.parkedLayer.Spatial == extPkt.VideoLayer.Spatial {
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
				if extPkt.VideoLayer.Spatial > s.currentLayer.Spatial && extPkt.VideoLayer.Spatial <= s.targetLayer.Spatial {
					s.logger.Infow(
						"upgrading layer",
						"current", s.currentLayer,
						"target", s.targetLayer,
						"max", s.maxLayer,
						"layer", extPkt.VideoLayer.Spatial,
						"req", s.requestSpatial,
						"maxSeen", s.maxSeenLayer,
						"feed", extPkt.Packet.SSRC,
					)
					found = true
				}

				if extPkt.VideoLayer.Spatial < s.currentLayer.Spatial && extPkt.VideoLayer.Spatial >= s.targetLayer.Spatial {
					s.logger.Infow(
						"downgrading layer",
						"current", s.currentLayer,
						"target", s.targetLayer,
						"max", s.maxLayer,
						"layer", extPkt.VideoLayer.Spatial,
						"req", s.requestSpatial,
						"maxSeen", s.maxSeenLayer,
						"feed", extPkt.Packet.SSRC,
					)
					found = true
				}

				if found {
					s.currentLayer.Spatial = extPkt.VideoLayer.Spatial
					/* RAJA-TODO
					if !f.isTemporalSupported {
						f.currentLayers.Temporal = f.targetLayers.Temporal
					}
					*/
				}
			}
		}

		if found {
			result.IsSwitchingLayer = true
			s.SetParked(buffer.InvalidLayers)
			if s.currentLayer.Spatial >= s.maxLayer.Spatial {
				result.IsSwitchingToMaxSpatial = true

				s.logger.Infow(
					"reached max layer",
					"current", s.currentLayer,
					"target", s.targetLayer,
					"max", s.maxLayer,
					"layer", extPkt.VideoLayer.Spatial,
					// RAJA-TODO "req", f.requestLayerSpatial,
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
		if extPkt.VideoLayer.Spatial <= s.maxLayer.Spatial && extPkt.KeyFrame {
			s.logger.Infow(
				"adjusting overshoot",
				"current", s.currentLayer,
				"target", s.targetLayer,
				"max", s.maxLayer,
				"layer", extPkt.VideoLayer.Spatial,
				// RAJA-TODO "req", f.requestLayerSpatial,
				"maxSeen", s.maxSeenLayer,
				"feed", extPkt.Packet.SSRC,
			)
			s.currentLayer.Spatial = extPkt.VideoLayer.Spatial

			if s.currentLayer.Spatial >= s.maxLayer.Spatial {
				result.IsSwitchingToMaxSpatial = true
			}

			if s.currentLayer.Spatial >= s.maxLayer.Spatial || s.currentLayer.Spatial == s.maxSeenLayer.Spatial {
				s.targetLayer.Spatial = extPkt.VideoLayer.Spatial
			}
		}
	}

	result.RTPMarker = extPkt.Packet.Marker
	result.IsSelected = extPkt.VideoLayer.Spatial == s.currentLayer.Spatial
	result.IsRelevant = false
	return
}
