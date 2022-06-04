package utils

import "github.com/livekit/protocol/livekit"

func SpatialLayerForQuality(quality livekit.VideoQuality) int32 {
	switch quality {
	case livekit.VideoQuality_LOW:
		return 0
	case livekit.VideoQuality_MEDIUM:
		return 1
	case livekit.VideoQuality_HIGH:
		return 2
	case livekit.VideoQuality_OFF:
		return -1
	default:
		return -1
	}
}
