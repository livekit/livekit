package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestTrackInfo(t *testing.T) {
	// ensures that persisted trackinfo is being returned
	ti := livekit.TrackInfo{
		Sid:       "testsid",
		Name:      "testtrack",
		Source:    livekit.TrackSource_SCREEN_SHARE,
		Type:      livekit.TrackType_VIDEO,
		Simulcast: false,
		Width:     100,
		Height:    80,
		Muted:     true,
	}

	mt := NewMediaTrack(MediaTrackParams{
		TrackInfo: &ti,
	})
	outInfo := mt.ToProto()
	require.Equal(t, ti.Muted, outInfo.Muted)
	require.Equal(t, ti.Name, outInfo.Name)
	require.Equal(t, ti.Name, mt.Name())
	require.Equal(t, livekit.TrackID(ti.Sid), mt.ID())
	require.Equal(t, ti.Type, outInfo.Type)
	require.Equal(t, ti.Type, mt.Kind())
	require.Equal(t, ti.Source, outInfo.Source)
	require.Equal(t, ti.Width, outInfo.Width)
	require.Equal(t, ti.Height, outInfo.Height)
	require.Equal(t, ti.Simulcast, outInfo.Simulcast)

	// make it simulcasted
	mt.simulcasted.Store(true)
	require.True(t, mt.ToProto().Simulcast)
}

func TestGetQualityForDimension(t *testing.T) {
	t.Run("landscape source", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{TrackInfo: &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Width:  1080,
			Height: 720,
		}})

		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(120, 120))
		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(300, 200))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(200, 250))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(700, 480))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(500, 1000))
	})

	t.Run("portrait source", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{TrackInfo: &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Width:  540,
			Height: 960,
		}})

		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(200, 400))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(400, 400))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(400, 700))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(600, 900))
	})

	t.Run("layers provided", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{TrackInfo: &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Width:  1080,
			Height: 720,
			Layers: []*livekit.VideoLayer{
				{
					Quality: livekit.VideoQuality_LOW,
					Width:   480,
					Height:  270,
				},
				{
					Quality: livekit.VideoQuality_MEDIUM,
					Width:   960,
					Height:  540,
				},
				{
					Quality: livekit.VideoQuality_HIGH,
					Width:   1080,
					Height:  720,
				},
			},
		}})

		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(120, 120))
		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(300, 300))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(800, 500))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(1000, 700))
	})
}
