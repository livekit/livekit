// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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

func NewSimulcastFromOther(vls VideoLayerSelector) *Simulcast {
	switch vls := vls.(type) {
	case *Null:
		return &Simulcast{
			Base: vls.Base,
		}

	case *Simulcast:
		return &Simulcast{
			Base: vls.Base,
		}

	case *DependencyDescriptor:
		return &Simulcast{
			Base: vls.Base,
		}

	case *VP9:
		return &Simulcast{
			Base: vls.Base,
		}

	default:
		return nil
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

		if reason != "" {
			s.logger.Debugw(
				reason,
				"previous", s.previousLayer,
				"current", s.currentLayer,
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

		// Two things to check when not locked to target
		//   1. Opportunistic layer upgrade - needs a key frame
		//   2. Need to downgrade - needs a key frame
		isActive := s.currentLayer.IsValid()
		found := false
		reason := ""
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

		if found {
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
