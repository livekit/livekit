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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
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

type revokeTestParticipant struct {
	types.LocalParticipant
	id         livekit.ParticipantID
	identity   livekit.ParticipantIdentity
	isRecorder bool
}

func (p *revokeTestParticipant) ID() livekit.ParticipantID { return p.id }
func (p *revokeTestParticipant) Identity() livekit.ParticipantIdentity {
	return p.identity
}
func (p *revokeTestParticipant) IsRecorder() bool { return p.isRecorder }

type revokeTestSubscribedTrack struct {
	types.SubscribedTrack
	subscriber *revokeTestParticipant
}

func (s *revokeTestSubscribedTrack) Subscriber() types.LocalParticipant { return s.subscriber }
func (s *revokeTestSubscribedTrack) SubscriberID() livekit.ParticipantID {
	return s.subscriber.id
}
func (s *revokeTestSubscribedTrack) SubscriberIdentity() livekit.ParticipantIdentity {
	return s.subscriber.identity
}
func (s *revokeTestSubscribedTrack) DownTrack() *sfu.DownTrack { return nil }

func newTestMediaTrackReceiverWithSubscribers(n int) *MediaTrackReceiver {
	mtr := NewMediaTrackReceiver(MediaTrackReceiverParams{
		MediaTrack: NewMediaTrack(MediaTrackParams{Logger: logger.GetLogger()}, &livekit.TrackInfo{
			Sid:  "TR_test",
			Type: livekit.TrackType_VIDEO,
		}),
		Logger: logger.GetLogger(),
	}, &livekit.TrackInfo{
		Sid:  "TR_test",
		Type: livekit.TrackType_VIDEO,
	})
	for i := range n {
		id := livekit.ParticipantID(fmt.Sprintf("PA_%04d", i))
		identity := livekit.ParticipantIdentity(fmt.Sprintf("sub_%04d", i))
		mtr.MediaTrackSubscriptions.subscribedTracks[id] = &revokeTestSubscribedTrack{
			subscriber: &revokeTestParticipant{id: id, identity: identity},
		}
	}
	return mtr
}

func newTestAllowedSubscriberIdentities(n int) []livekit.ParticipantIdentity {
	allowed := make([]livekit.ParticipantIdentity, 0, n)
	for i := range n {
		allowed = append(allowed, livekit.ParticipantIdentity(fmt.Sprintf("sub_%04d", i)))
	}
	return allowed
}

func TestMediaTrackReceiverRevokeDisallowedSubscribers(t *testing.T) {
	t.Run("empty subscribers", func(t *testing.T) {
		mtr := newTestMediaTrackReceiverWithSubscribers(0)
		revoked := mtr.RevokeDisallowedSubscribers([]livekit.ParticipantIdentity{"sub_0000"})
		require.Empty(t, revoked)
	})

	t.Run("single subscriber with large allowlist", func(t *testing.T) {
		mtr := newTestMediaTrackReceiverWithSubscribers(1)
		revoked := mtr.RevokeDisallowedSubscribers(newTestAllowedSubscriberIdentities(15))
		require.Empty(t, revoked)
	})

	t.Run("small allowlist and exempt recorder", func(t *testing.T) {
		mtr := newTestMediaTrackReceiverWithSubscribers(2)
		mtr.MediaTrackSubscriptions.subscribedTracks["PA_rec"] = &revokeTestSubscribedTrack{
			subscriber: &revokeTestParticipant{id: "PA_rec", identity: "rec", isRecorder: true},
		}
		revoked := mtr.RevokeDisallowedSubscribers([]livekit.ParticipantIdentity{"sub_0000"})
		require.Equal(t, []livekit.ParticipantIdentity{"sub_0001"}, revoked)
	})

	t.Run("large allowlist above map threshold", func(t *testing.T) {
		mtr := newTestMediaTrackReceiverWithSubscribers(20)
		mtr.MediaTrackSubscriptions.subscribedTracks["PA_rec"] = &revokeTestSubscribedTrack{
			subscriber: &revokeTestParticipant{id: "PA_rec", identity: "rec", isRecorder: true},
		}
		revoked := mtr.RevokeDisallowedSubscribers(newTestAllowedSubscriberIdentities(15))
		expected := []livekit.ParticipantIdentity{"sub_0015", "sub_0016", "sub_0017", "sub_0018", "sub_0019"}
		require.ElementsMatch(t, expected, revoked)
	})
}

func BenchmarkMediaTrackReceiverRevokeDisallowedSubscribers(b *testing.B) {
	for _, size := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("subscribers_%d", size), func(b *testing.B) {
			mtr := newTestMediaTrackReceiverWithSubscribers(size)
			allowed := newTestAllowedSubscriberIdentities(size)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = mtr.RevokeDisallowedSubscribers(allowed)
			}
		})
	}
}
