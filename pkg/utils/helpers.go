package utils

import (
	"sort"

	"github.com/livekit/protocol/livekit"
)

const (
	layerSelectionTolerance = 0.9
)

func GetQualityForDimension(width, height uint32) livekit.VideoQuality {
	quality := livekit.VideoQuality_HIGH
	if t.Kind() == livekit.TrackType_AUDIO {
		return quality
	}

	t.lock.RLock()
	defer t.lock.RUnlock()

	if t.trackInfo.Height == 0 {
		return quality
	}
	origSize := t.trackInfo.Height
	requestedSize := height
	if t.trackInfo.Width < t.trackInfo.Height {
		// for portrait videos
		origSize = t.trackInfo.Width
		requestedSize = width
	}

	// default sizes representing qualities low - high
	layerSizes := []uint32{180, 360, origSize}
	var providedSizes []uint32
	for _, layer := range t.layerDimensions {
		providedSizes = append(providedSizes, layer.Height)
	}
	if len(providedSizes) > 0 {
		layerSizes = providedSizes
		// comparing height always
		requestedSize = height
		sort.Slice(layerSizes, func(i, j int) bool {
			return layerSizes[i] < layerSizes[j]
		})
	}

	// finds the lowest layer that could satisfy client demands
	requestedSize = uint32(float32(requestedSize) * layerSelectionTolerance)
	for i, s := range layerSizes {
		quality = livekit.VideoQuality(i)
		if s >= requestedSize {
			break
		}
	}

	return quality
}

func SpatialLayerForQuality(quality livekit.VideoQuality, videoLayers map[livekit.Quality]*livekit.VideoLayer) int32 {
	if len(videoLayers) == 0 || len(videoLayers) > 2 {
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

	switch len(videoLayers) {
	case 1:
		// only one layer - always map it to layer 0
		return 0
	case 2:
		switch quality {
		case livekit.VideoQuality_LOW:
			return 0
		case livekit.VideoQuality_MEDIUM:
			// RAJA-TODO: map the two layers client is sending to server's notion of LOW/MEDIUM/HIGH which server calculates
			// and then figure out the two layers client sends to what server understands
			if videoLayers[livekit.VideoQuality_LOW] != nil && videoLayers[livekit.VideoQuality_MEDIUM] != nil {
				return 0
			}
			if videoLayers[livekit.VideoQuality_LOW] != nil && videoLayers[livekit.VideoQuality_HIGH] != nil {
				return 1
			}
			if videoLayers[livekit.VideoQuality_MEDIUM] != nil && videoLayers[livekit.VideoQuality_HIGH] != nil {
				return 1
			}
		case livekit.VideoQuality_HIGH:
			highestLayer := -1
			for q := livekit.VideoQuality_LOW; q <= livekit.VideoQuality_HIGH; q++ {
				if l, ok := videoLayers[q]; ok && l > highestLayer {
					highestLayer = l
				}
			}
			return highestLayer
		}
	}

	return -1
}

func QualityForSpatialLayer(layer int32) livekit.VideoQuality {
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
