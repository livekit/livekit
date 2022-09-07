package buffer

import (
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
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
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
				FullResolution:    RidAndLayer{rid: FullResolution, layer: 2},
			},
		},
		{
			"no layers",
			&livekit.TrackInfo{},
			map[string]RidAndLayer{
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
				FullResolution:    RidAndLayer{rid: FullResolution, layer: 2},
			},
		},
		{
			"single layer, low",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
				},
			},
			map[string]RidAndLayer{
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: QuarterResolution, layer: 0},
				FullResolution:    RidAndLayer{rid: QuarterResolution, layer: 0},
			},
		},
		{
			"single layer, medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[string]RidAndLayer{
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: QuarterResolution, layer: 0},
				FullResolution:    RidAndLayer{rid: QuarterResolution, layer: 0},
			},
		},
		{
			"single layer, high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[string]RidAndLayer{
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: QuarterResolution, layer: 0},
				FullResolution:    RidAndLayer{rid: QuarterResolution, layer: 0},
			},
		},
		{
			"two layers, low and medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[string]RidAndLayer{
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
				FullResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
			},
		},
		{
			"two layers, low and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[string]RidAndLayer{
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
				FullResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
			},
		},
		{
			"two layers, medium and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[string]RidAndLayer{
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
				FullResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
			},
		},
		{
			"three layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[string]RidAndLayer{
				"":                RidAndLayer{rid: QuarterResolution, layer: 0},
				QuarterResolution: RidAndLayer{rid: QuarterResolution, layer: 0},
				HalfResolution:    RidAndLayer{rid: HalfResolution, layer: 1},
				FullResolution:    RidAndLayer{rid: FullResolution, layer: 2},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for testRid, expectedResult := range test.ridToLayer {
				actualLayer := RidToSpatialLayer(testRid, test.trackInfo)
				require.Equal(t, expectedResult.layer, actualLayer)

				actualRid := SpatialLayerToRid(actualLayer, test.trackInfo)
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
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 1},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 2},
			},
		},
		{
			"no layers",
			&livekit.TrackInfo{},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 1},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 2},
			},
		},
		{
			"single layer, low",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_LOW, layer: 0},
			},
		},
		{
			"single layer, medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 0},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 0},
			},
		},
		{
			"single layer, high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 0},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 0},
			},
		},
		{
			"two layers, low and medium",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 1},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 1},
			},
		},
		{
			"two layers, low and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 1},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 1},
			},
		},
		{
			"two layers, medium and high",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 0},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 1},
			},
		},
		{
			"three layers",
			&livekit.TrackInfo{
				Layers: []*livekit.VideoLayer{
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
				},
			},
			map[livekit.VideoQuality]QualityAndLayer{
				livekit.VideoQuality_LOW:    QualityAndLayer{quality: livekit.VideoQuality_LOW, layer: 0},
				livekit.VideoQuality_MEDIUM: QualityAndLayer{quality: livekit.VideoQuality_MEDIUM, layer: 1},
				livekit.VideoQuality_HIGH:   QualityAndLayer{quality: livekit.VideoQuality_HIGH, layer: 2},
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
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
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
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
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
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
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
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
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
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
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
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
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
					&livekit.VideoLayer{Quality: livekit.VideoQuality_LOW},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_MEDIUM},
					&livekit.VideoLayer{Quality: livekit.VideoQuality_HIGH},
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
				actualRid := VideoQualityToRid(testQuality, test.trackInfo)
				require.Equal(t, expectedRid, actualRid)
			}
		})
	}
}
