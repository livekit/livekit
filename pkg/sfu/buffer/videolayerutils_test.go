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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestRidConversion(t *testing.T) {
	type RidAndLayer struct {
		rid   string
		layer int32
	}
	tests := []struct {
		name       string
		trackInfo  *livekit.TrackInfo
		ridToLayer map[string]RidAndLayer
	}{
		{
			"no track info",
			nil,
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: HalfResolution, layer: 1},
				FullResolution:    {rid: FullResolution, layer: 2},
			},
		},
		{
			"no layers",
			&livekit.TrackInfo{},
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: HalfResolution, layer: 1},
				FullResolution:    {rid: FullResolution, layer: 2},
			},
		},
		{
			"single layer, low",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
				},
			},
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: QuarterResolution, layer: 0},
				FullResolution:    {rid: QuarterResolution, layer: 0},
			},
		},
		{
			"single layer, medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: QuarterResolution, layer: 0},
				FullResolution:    {rid: QuarterResolution, layer: 0},
			},
		},
		{
			"single layer, high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: QuarterResolution, layer: 0},
				FullResolution:    {rid: QuarterResolution, layer: 0},
			},
		},
		{
			"two layers, low and medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: HalfResolution, layer: 1},
				FullResolution:    {rid: HalfResolution, layer: 1},
			},
		},
		{
			"two layers, low and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: HalfResolution, layer: 1},
				FullResolution:    {rid: HalfResolution, layer: 1},
			},
		},
		{
			"two layers, medium and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_MEDIUM},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: HalfResolution, layer: 1},
				FullResolution:    {rid: HalfResolution, layer: 1},
			},
		},
		{
			"three layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_MEDIUM},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[string]RidAndLayer{
				"":                {rid: QuarterResolution, layer: 0},
				QuarterResolution: {rid: QuarterResolution, layer: 0},
				HalfResolution:    {rid: HalfResolution, layer: 1},
				FullResolution:    {rid: FullResolution, layer: 2},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for testRid, expectedResult := range test.ridToLayer {
				actualLayer := RidToSpatialLayer(testRid, test.trackInfo, DefaultVideoLayersRid)
				require.Equal(t, expectedResult.layer, actualLayer)

				actualRid := SpatialLayerToRid(actualLayer, test.trackInfo, DefaultVideoLayersRid)
				require.Equal(t, expectedResult.rid, actualRid)
			}
		})
	}
}

func TestQualityConversion(t *testing.T) {
	type QualityAndLayer struct {
		quality livekit.VideoQuality
		layer   int32
	}
	tests := []struct {
		name           string
		trackInfo      *livekit.TrackInfo
		qualityToLayer map[livekit.VideoQuality]QualityAndLayer
	}{
		{
			"no track info",
			nil,
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_MEDIUM, layer: 1},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_HIGH, layer: 2},
			},
		},
		{
			"no layers",
			&livekit.TrackInfo{},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_MEDIUM, layer: 1},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_HIGH, layer: 2},
			},
		},
		{
			"single layer, low",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_LOW, layer: 0},
			},
		},
		{
			"single layer, medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_MEDIUM, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_MEDIUM, layer: 0},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_MEDIUM, layer: 0},
			},
		},
		{
			"single layer, high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_HIGH, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_HIGH, layer: 0},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_HIGH, layer: 0},
			},
		},
		{
			"two layers, low and medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_MEDIUM, layer: 1},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_MEDIUM, layer: 1},
			},
		},
		{
			"two layers, low and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_HIGH, layer: 1},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_HIGH, layer: 1},
			},
		},
		{
			"two layers, medium and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_MEDIUM},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_MEDIUM, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_MEDIUM, layer: 0},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_HIGH, layer: 1},
			},
		},
		{
			"three layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_MEDIUM},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    {quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: {quality: livekit.VideoQuality_MEDIUM, layer: 1},
				livekit.VideoQuality_HIGH:   {quality: livekit.VideoQuality_HIGH, layer: 2},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for testQuality, expectedResult := range test.qualityToLayer {
				actualLayer := VideoQualityToSpatialLayer(testQuality, test.trackInfo)
				require.Equal(t, expectedResult.layer, actualLayer)

				actualQuality := SpatialLayerToVideoQuality(actualLayer, test.trackInfo)
				require.Equal(t, expectedResult.quality, actualQuality)
			}
		})
	}
}

func TestVideoQualityToRidConversion(t *testing.T) {
	tests := []struct {
		name         string
		trackInfo    *livekit.TrackInfo
		qualityToRid map[livekit.VideoQuality]string
	}{
		{
			"no track info",
			nil,
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: HalfResolution,
				livekit.VideoQuality_HIGH:   FullResolution,
			},
		},
		{
			"no layers",
			&livekit.TrackInfo{},
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: HalfResolution,
				livekit.VideoQuality_HIGH:   FullResolution,
			},
		},
		{
			"single layer, low",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
				},
			},
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: QuarterResolution,
				livekit.VideoQuality_HIGH:   QuarterResolution,
			},
		},
		{
			"single layer, medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: QuarterResolution,
				livekit.VideoQuality_HIGH:   QuarterResolution,
			},
		},
		{
			"single layer, high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: QuarterResolution,
				livekit.VideoQuality_HIGH:   QuarterResolution,
			},
		},
		{
			"two layers, low and medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: HalfResolution,
				livekit.VideoQuality_HIGH:   HalfResolution,
			},
		},
		{
			"two layers, low and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: HalfResolution,
				livekit.VideoQuality_HIGH:   HalfResolution,
			},
		},
		{
			"two layers, medium and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_MEDIUM},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: QuarterResolution,
				livekit.VideoQuality_HIGH:   HalfResolution,
			},
		},
		{
			"three layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW},
					{Quality: livekit.VideoQuality_MEDIUM},
					{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]string{
				livekit.VideoQuality_LOW:    QuarterResolution,
				livekit.VideoQuality_MEDIUM: HalfResolution,
				livekit.VideoQuality_HIGH:   FullResolution,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for testQuality, expectedRid := range test.qualityToRid {
				actualRid := VideoQualityToRid(testQuality, test.trackInfo, DefaultVideoLayersRid)
				require.Equal(t, expectedRid, actualRid)
			}
		})
	}
}

func TestGetSpatialLayerForRid(t *testing.T) {
	tests := []struct {
		name              string
		trackInfo         *livekit.TrackInfo
		ridToSpatialLayer map[string]int32
	}{
		{
			"no track info",
			nil,
			map[string]int32{
				QuarterResolution: InvalidLayerSpatial,
				HalfResolution:    InvalidLayerSpatial,
				FullResolution:    InvalidLayerSpatial,
			},
		},
		{
			"no layers",
			&livekit.TrackInfo{},
			map[string]int32{
				QuarterResolution: InvalidLayerSpatial,
				HalfResolution:    InvalidLayerSpatial,
				FullResolution:    InvalidLayerSpatial,
			},
		},
		{
			"no rid",
			&livekit.TrackInfo{},
			map[string]int32{
				"": 0,
			},
		},
		{
			"single layer",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW, SpatialLayer: 0},
				},
			},
			map[string]int32{
				QuarterResolution: 0,
				HalfResolution:    0,
				FullResolution:    0,
			},
		},
		{
			"layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW, SpatialLayer: 0, Rid: QuarterResolution},
					{Quality: livekit.VideoQuality_MEDIUM, SpatialLayer: 1, Rid: HalfResolution},
				},
			},
			map[string]int32{
				QuarterResolution: 0,
				HalfResolution:    1,
				FullResolution:    InvalidLayerSpatial,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for testRid, expectedSpatialLayer := range test.ridToSpatialLayer {
				actualSpatialLayer := GetSpatialLayerForRid(testRid, test.trackInfo)
				require.Equal(t, expectedSpatialLayer, actualSpatialLayer)
			}
		})
	}
}

func TestGetSpatialLayerForVideoQuality(t *testing.T) {
	tests := []struct {
		name                       string
		trackInfo                  *livekit.TrackInfo
		videoQualityToSpatialLayer map[livekit.VideoQuality]int32
	}{
		{
			"no track info",
			nil,
			map[livekit.VideoQuality]int32{
				livekit.VideoQuality_LOW:    InvalidLayerSpatial,
				livekit.VideoQuality_MEDIUM: InvalidLayerSpatial,
				livekit.VideoQuality_HIGH:   InvalidLayerSpatial,
				livekit.VideoQuality_OFF:    InvalidLayerSpatial,
			},
		},
		{
			"no layers",
			&livekit.TrackInfo{},
			map[livekit.VideoQuality]int32{
				livekit.VideoQuality_LOW:    0,
				livekit.VideoQuality_MEDIUM: 0,
				livekit.VideoQuality_HIGH:   0,
				livekit.VideoQuality_OFF:    InvalidLayerSpatial,
			},
		},
		{
			"not all layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW, SpatialLayer: 0, Rid: QuarterResolution},
					{Quality: livekit.VideoQuality_MEDIUM, SpatialLayer: 1, Rid: HalfResolution},
				},
			},
			map[livekit.VideoQuality]int32{
				livekit.VideoQuality_LOW:    0,
				livekit.VideoQuality_MEDIUM: 1,
				livekit.VideoQuality_HIGH:   1,
				livekit.VideoQuality_OFF:    InvalidLayerSpatial,
			},
		},
		{
			"all layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW, SpatialLayer: 0, Rid: QuarterResolution},
					{Quality: livekit.VideoQuality_MEDIUM, SpatialLayer: 1, Rid: HalfResolution},
					{Quality: livekit.VideoQuality_HIGH, SpatialLayer: 2, Rid: FullResolution},
				},
			},
			map[livekit.VideoQuality]int32{
				livekit.VideoQuality_LOW:    0,
				livekit.VideoQuality_MEDIUM: 1,
				livekit.VideoQuality_HIGH:   2,
				livekit.VideoQuality_OFF:    InvalidLayerSpatial,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for testVideoQuality, expectedSpatialLayer := range test.videoQualityToSpatialLayer {
				actualSpatialLayer := GetSpatialLayerForVideoQuality(testVideoQuality, test.trackInfo)
				require.Equal(t, expectedSpatialLayer, actualSpatialLayer)
			}
		})
	}
}

func TestGetVideoQualityorSpatialLayer(t *testing.T) {
	tests := []struct {
		name                       string
		trackInfo                  *livekit.TrackInfo
		spatialLayerToVideoQuality map[int32]livekit.VideoQuality
	}{
		{
			"no track info",
			nil,
			map[int32]livekit.VideoQuality{
				InvalidLayerSpatial: livekit.VideoQuality_OFF,
				0:                   livekit.VideoQuality_OFF,
				1:                   livekit.VideoQuality_OFF,
				2:                   livekit.VideoQuality_OFF,
			},
		},
		{
			"no layers",
			&livekit.TrackInfo{},
			map[int32]livekit.VideoQuality{
				InvalidLayerSpatial: livekit.VideoQuality_OFF,
				0:                   livekit.VideoQuality_OFF,
				1:                   livekit.VideoQuality_OFF,
				2:                   livekit.VideoQuality_OFF,
			},
		},
		{
			"layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					{Quality: livekit.VideoQuality_LOW, SpatialLayer: 0, Rid: QuarterResolution},
					{Quality: livekit.VideoQuality_MEDIUM, SpatialLayer: 1, Rid: HalfResolution},
				},
			},
			map[int32]livekit.VideoQuality{
				InvalidLayerSpatial: livekit.VideoQuality_OFF,
				0:                   livekit.VideoQuality_LOW,
				1:                   livekit.VideoQuality_MEDIUM,
				2:                   livekit.VideoQuality_OFF,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for testSpatialLayer, expectedVideoQuality := range test.spatialLayerToVideoQuality {
				actualVideoQuality := GetVideoQualityForSpatialLayer(testSpatialLayer, test.trackInfo)
				require.Equal(t, expectedVideoQuality, actualVideoQuality)
			}
		})
	}
}
