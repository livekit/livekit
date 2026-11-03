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

package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/codec"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
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

	mt := NewMediaTrack(MediaTrackParams{}, &ti)
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
}

func TestGetQualityForDimension(t *testing.T) {
	t.Run("landscape source", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{
			Logger: logger.GetLogger(),
		}, &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Width:  1080,
			Height: 720,
		})

		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(mime.MimeTypeVP8, 120, 120))
		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(mime.MimeTypeVP8, 300, 200))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(mime.MimeTypeVP8, 200, 250))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeVP8, 700, 480))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeVP8, 500, 1000))
	})

	t.Run("portrait source", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{
			Logger: logger.GetLogger(),
		}, &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Width:  540,
			Height: 960,
		})

		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(mime.MimeTypeVP8, 200, 400))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(mime.MimeTypeVP8, 400, 400))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(mime.MimeTypeVP8, 400, 700))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeVP8, 600, 900))
	})

	t.Run("layers provided", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{
			Logger: logger.GetLogger(),
		}, &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Width:  1080,
			Height: 720,
			Codecs: []*livekit.SimulcastCodecInfo{
				{
					MimeType: mime.MimeTypeH264.String(),
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
				},
			},
		})

		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(mime.MimeTypeH264, 120, 120))
		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(mime.MimeTypeH264, 300, 300))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(mime.MimeTypeH264, 800, 500))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeH264, 1000, 700))
	})

	t.Run("highest layer with smallest dimensions", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{
			Logger: logger.GetLogger(),
		}, &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Width:  1080,
			Height: 720,
			Codecs: []*livekit.SimulcastCodecInfo{
				{
					MimeType: mime.MimeTypeH264.String(),
					Layers: []*livekit.VideoLayer{
						{
							Quality: livekit.VideoQuality_LOW,
							Width:   480,
							Height:  270,
						},
						{
							Quality: livekit.VideoQuality_MEDIUM,
							Width:   1080,
							Height:  720,
						},
						{
							Quality: livekit.VideoQuality_HIGH,
							Width:   1080,
							Height:  720,
						},
					},
				},
			},
		})

		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(mime.MimeTypeH264, 120, 120))
		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(mime.MimeTypeH264, 300, 300))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeH264, 800, 500))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeH264, 1000, 700))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeH264, 1200, 800))

		mt = NewMediaTrack(MediaTrackParams{
			Logger: logger.GetLogger(),
		}, &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Width:  1080,
			Height: 720,
			Codecs: []*livekit.SimulcastCodecInfo{
				{
					MimeType: mime.MimeTypeH264.String(),
					Layers: []*livekit.VideoLayer{
						{
							Quality: livekit.VideoQuality_LOW,
							Width:   480,
							Height:  270,
						},
						{
							Quality: livekit.VideoQuality_MEDIUM,
							Width:   480,
							Height:  270,
						},
						{
							Quality: livekit.VideoQuality_HIGH,
							Width:   1080,
							Height:  720,
						},
					},
				},
			},
		})

		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(mime.MimeTypeH264, 120, 120))
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(mime.MimeTypeH264, 300, 300))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeH264, 800, 500))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeH264, 1000, 700))
		require.Equal(t, livekit.VideoQuality_HIGH, mt.GetQualityForDimension(mime.MimeTypeH264, 1200, 800))
	})

}

func TestUpdateVideoSize(t *testing.T) {
	newTrack := func() *MediaTrack {
		return NewMediaTrack(MediaTrackParams{
			Logger:            logger.GetLogger(),
			ParticipantID:     func() livekit.ParticipantID { return "PA_test" },
			TelemetryListener: func() types.ParticipantTelemetryListener { return types.NullParticipantTelemetryListener{} },
		}, &livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Source: livekit.TrackSource_SCREEN_SHARE,
			Width:  1920,
			Height: 1080,
			Codecs: []*livekit.SimulcastCodecInfo{
				{
					MimeType: mime.MimeTypeVP8.String(),
					Layers: []*livekit.VideoLayer{
						{Quality: livekit.VideoQuality_LOW, Width: 960, Height: 540},
						{Quality: livekit.VideoQuality_HIGH, Width: 1920, Height: 1080},
					},
				},
			},
		})
	}

	t.Run("measured sizes replace the announced ones", func(t *testing.T) {
		mt := newTrack()
		require.Equal(t, livekit.VideoQuality_LOW, mt.GetQualityForDimension(mime.MimeTypeVP8, 889, 500))

		mt.UpdateVideoSize(mime.MimeTypeVP8, []codec.VideoSize{{Width: 834, Height: 407}, {Width: 1668, Height: 814}})

		info := mt.ToProto()
		require.Equal(t, uint32(1668), info.Width)
		require.Equal(t, uint32(814), info.Height)
		require.Equal(t, uint32(834), info.Codecs[0].Layers[0].Width)
		require.Equal(t, uint32(407), info.Codecs[0].Layers[0].Height)
		require.Equal(t, livekit.VideoQuality_MEDIUM, mt.GetQualityForDimension(mime.MimeTypeVP8, 889, 500))
	})

	t.Run("fewer measured sizes than announced layers change nothing", func(t *testing.T) {
		mt := newTrack()

		mt.UpdateVideoSize(mime.MimeTypeVP8, []codec.VideoSize{{Width: 834, Height: 407}})

		info := mt.ToProto()
		require.Equal(t, uint32(1920), info.Width)
		require.Equal(t, uint32(1080), info.Height)
		require.Equal(t, uint32(960), info.Codecs[0].Layers[0].Width)
		require.Equal(t, uint32(540), info.Codecs[0].Layers[0].Height)
	})
}
