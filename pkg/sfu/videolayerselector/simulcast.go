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
	populateSwitches := func(isActive bool, reason string) {
		result.IsSwitching = true
		if !isActive {
			result.IsResuming = true
		}

		if s.currentLayer.Spatial == s.requestSpatial {
			result.IsSwitchingToRequestSpatial = true
		}

		if s.currentLayer.Spatial >= s.maxLayer.Spatial {
			result.IsSwitchingToMaxSpatial = true
			result.MaxSpatialLayer = s.currentLayer.Spatial
			if reason != "" {
				reason += ", "
			}
			reason += "reached max layer"
		}

		if reason != "" {
			s.logger.Infow(
				reason,
				"previous", s.previousLayer,
				"current", s.currentLayer,
				"previousParked", s.previousParkedLayer,
				"parked", s.parkedLayer,
				"previousTarget", s.previousTargetLayer,
				"target", s.targetLayer,
				"max", s.maxLayer,
				"layer", layer,
				"req", s.requestSpatial,
				"maxSeen", s.maxSeenLayer,
				"feed", extPkt.Packet.SSRC,
			)
		}
	}

	if s.currentLayer.Spatial != s.targetLayer.Spatial {
		currentLayer := s.currentLayer

		// Three things to check when not locked to target
		//   1. Resumable layer - don't need a key frame
		//   2. Opportunistic layer upgrade - needs a key frame
		//   3. Need to downgrade - needs a key frame
		isActive := s.currentLayer.IsValid()
		found := false
		reason := ""
		if s.parkedLayer.IsValid() {
			if s.parkedLayer.Spatial == layer {
				reason = "resuming at parked layer"
				currentLayer = s.parkedLayer
				found = true
			}
		} else {
			if extPkt.KeyFrame {
				if layer > s.currentLayer.Spatial && layer <= s.targetLayer.Spatial {
					reason = "upgrading layer"
					found = true
				}

				if layer < s.currentLayer.Spatial && layer >= s.targetLayer.Spatial {
					reason = "downgrading layer"
					found = true
				}

				if found {
					currentLayer.Spatial = layer
					currentLayer.Temporal = extPkt.VideoLayer.Temporal
				}
			}
		}

		if found {
			s.previousParkedLayer = s.parkedLayer
			s.parkedLayer = buffer.InvalidLayer

			s.previousLayer = s.currentLayer
			s.currentLayer = currentLayer

			s.previousTargetLayer = s.targetLayer
			if s.currentLayer.Spatial >= s.maxLayer.Spatial || s.currentLayer.Spatial == s.maxSeenLayer.Spatial {
				s.targetLayer.Spatial = s.currentLayer.Spatial
			}

			populateSwitches(isActive, reason)
		}
	}

	// if locked to higher than max layer due to overshoot, check if it can be dialed back
	if s.currentLayer.Spatial > s.maxLayer.Spatial && layer <= s.maxLayer.Spatial && extPkt.KeyFrame {
		s.previousLayer = s.currentLayer
		s.currentLayer.Spatial = layer

		s.previousTargetLayer = s.targetLayer
		if s.currentLayer.Spatial >= s.maxLayer.Spatial || s.currentLayer.Spatial == s.maxSeenLayer.Spatial {
			s.targetLayer.Spatial = layer
		}

		populateSwitches(true, "adjusting overshoot")
	}

	result.RTPMarker = extPkt.Packet.Marker
	result.IsSelected = layer == s.currentLayer.Spatial
	result.IsRelevant = false
	return
}
