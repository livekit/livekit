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

	"github.com/pion/transport/v5/packetio"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
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

type codecOnlyTrackRemote struct {
	codec webrtc.RTPCodecParameters
}

func (r *codecOnlyTrackRemote) ID() string                       { return "webrtc-track" }
func (r *codecOnlyTrackRemote) RID() string                      { return "" }
func (r *codecOnlyTrackRemote) Msid() string                     { return "" }
func (r *codecOnlyTrackRemote) SSRC() webrtc.SSRC                { return 1234 }
func (r *codecOnlyTrackRemote) RtxSSRC() webrtc.SSRC             { return 0 }
func (r *codecOnlyTrackRemote) StreamID() string                 { return "stream" }
func (r *codecOnlyTrackRemote) Kind() webrtc.RTPCodecType        { return webrtc.RTPCodecTypeVideo }
func (r *codecOnlyTrackRemote) Codec() webrtc.RTPCodecParameters { return r.codec }
func (r *codecOnlyTrackRemote) RTCTrack() *webrtc.TrackRemote    { return nil }

func TestAddReceiverCodecNotInTrackInfo(t *testing.T) {
	bufferFactory := buffer.NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()
	bufferFactory.GetOrNew(packetio.RTPBufferPacket, 1234)
	bufferFactory.GetOrNew(packetio.RTCPBufferPacket, 1234)

	mt := NewMediaTrack(MediaTrackParams{
		BufferFactory:      bufferFactory,
		Logger:             logger.GetLogger(),
		ShouldRegressCodec: func() bool { return false },
	}, &livekit.TrackInfo{
		Sid:  "TR_video",
		Type: livekit.TrackType_VIDEO,
		Codecs: []*livekit.SimulcastCodecInfo{
			{MimeType: mime.MimeTypeH264.String()},
			{MimeType: mime.MimeTypeVP8.String()},
		},
	})

	// the client sends a codec that the track did not list
	_, err := mt.AddReceiver(nil, &codecOnlyTrackRemote{
		codec: webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime.MimeTypeH265.String(), ClockRate: 90000},
			PayloadType:        49,
		},
	}, "0")
	require.ErrorIs(t, err, ErrReceiverCodecNotFound)
	require.Empty(t, mt.Receivers())
	require.True(t, isTrackUnforwardable(mt, err))

	// a failure that affects only one layer does not make the track unforwardable
	require.False(t, isTrackUnforwardable(mt, ErrReceiverNegativeLayer))
}
