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

package buffer

import (
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	QuarterResolution = "q"
	HalfResolution    = "h"
	FullResolution    = "f"
)

func LayerPresenceFromTrackInfo(trackInfo *livekit.TrackInfo) *[livekit.VideoQuality_HIGH + 1]bool {
	if trackInfo == nil || len(trackInfo.Layers) == 0 {
		return nil
	}

	var layerPresence [livekit.VideoQuality_HIGH + 1]bool
	for _, layer := range trackInfo.Layers {
		// WARNING: comparing protobuf enum
		if layer.Quality <= livekit.VideoQuality_HIGH {
			layerPresence[layer.Quality] = true
		} else {
			logger.Warnw("unexpected quality in track info", nil, "trackInfo", logger.Proto(trackInfo))
		}
	}

	return &layerPresence
}

func RidToSpatialLayer(rid string, trackInfo *livekit.TrackInfo) int32 {
	lp := LayerPresenceFromTrackInfo(trackInfo)
	if lp == nil {
		switch rid {
		case QuarterResolution:
			return 0
		case HalfResolution:
			return 1
		case FullResolution:
			return 2
		default:
			return 0
		}
	}

	switch rid {
	case QuarterResolution:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return 0

		default:
			// only one quality published, could be any
			return 0
		}

	case HalfResolution:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return 1

		default:
			// only one quality published, could be any
			return 0
		}

	case FullResolution:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return 2

		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			logger.Warnw("unexpected rid f with only two qualities, low and medium", nil)
			return 1
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			logger.Warnw("unexpected rid f with only two qualities, low and high", nil)
			return 1
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			logger.Warnw("unexpected rid f with only two qualities, medium and high", nil)
			return 1

		default:
			// only one quality published, could be any
			return 0
		}

	default:
		// no rid, should be single layer
		return 0
	}
}

func SpatialLayerToRid(layer int32, trackInfo *livekit.TrackInfo) string {
	lp := LayerPresenceFromTrackInfo(trackInfo)
	if lp == nil {
		switch layer {
		case 0:
			return QuarterResolution
		case 1:
			return HalfResolution
		case 2:
			return FullResolution
		default:
			return QuarterResolution
		}
	}

	switch layer {
	case 0:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return QuarterResolution

		default:
			return QuarterResolution
		}

	case 1:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return HalfResolution

		default:
			return QuarterResolution
		}

	case 2:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return FullResolution

		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			logger.Warnw("unexpected layer 2 with only two qualities, low and medium", nil)
			return HalfResolution
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			logger.Warnw("unexpected layer 2 with only two qualities, low and high", nil)
			return HalfResolution
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			logger.Warnw("unexpected layer 2 with only two qualities, medium and high", nil)
			return HalfResolution

		default:
			return QuarterResolution
		}

	default:
		return QuarterResolution
	}
}

func VideoQualityToRid(quality livekit.VideoQuality, trackInfo *livekit.TrackInfo) string {
	return SpatialLayerToRid(VideoQualityToSpatialLayer(quality, trackInfo), trackInfo)
}

func SpatialLayerToVideoQuality(layer int32, trackInfo *livekit.TrackInfo) livekit.VideoQuality {
	lp := LayerPresenceFromTrackInfo(trackInfo)
	if lp == nil {
		switch layer {
		case 0:
			return livekit.VideoQuality_LOW
		case 1:
			return livekit.VideoQuality_MEDIUM
		case 2:
			return livekit.VideoQuality_HIGH
		default:
			return livekit.VideoQuality_OFF
		}
	}

	switch layer {
	case 0:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW]:
			return livekit.VideoQuality_LOW

		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_MEDIUM]:
			return livekit.VideoQuality_MEDIUM

		default:
			return livekit.VideoQuality_HIGH
		}

	case 1:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			return livekit.VideoQuality_MEDIUM

		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return livekit.VideoQuality_HIGH

		default:
			logger.Errorw("invalid layer", nil, "layer", layer, "trackInfo", trackInfo)
			return livekit.VideoQuality_HIGH
		}

	case 2:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return livekit.VideoQuality_HIGH

		default:
			logger.Errorw("invalid layer", nil, "layer", layer, "trackInfo", trackInfo)
			return livekit.VideoQuality_HIGH
		}
	}

	return livekit.VideoQuality_OFF
}

func VideoQualityToSpatialLayer(quality livekit.VideoQuality, trackInfo *livekit.TrackInfo) int32 {
	lp := LayerPresenceFromTrackInfo(trackInfo)
	if lp == nil {
		switch quality {
		case livekit.VideoQuality_LOW:
			return 0
		case livekit.VideoQuality_MEDIUM:
			return 1
		case livekit.VideoQuality_HIGH:
			return 2
		default:
			return InvalidLayerSpatial
		}
	}

	switch quality {
	case livekit.VideoQuality_LOW:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		default: // only one quality published, could be any
			return 0
		}

	case livekit.VideoQuality_MEDIUM:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			return 1

		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return 0

		default: // only one quality published, could be any
			return 0
		}

	case livekit.VideoQuality_HIGH:
		switch {
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return 2

		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_MEDIUM]:
			fallthrough
		case lp[livekit.VideoQuality_LOW] && lp[livekit.VideoQuality_HIGH]:
			fallthrough
		case lp[livekit.VideoQuality_MEDIUM] && lp[livekit.VideoQuality_HIGH]:
			return 1

		default: // only one quality published, could be any
			return 0
		}
	}

	return InvalidLayerSpatial
}
