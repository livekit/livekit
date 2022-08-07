package rtc

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
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

func TestSubscribedMaxQuality(t *testing.T) {
	subscribedCodecsAsString := func(c1 []*livekit.SubscribedCodec) string {
		sort.Slice(c1, func(i, j int) bool { return c1[i].Codec < c1[j].Codec })
		var s1 string
		for _, c := range c1 {
			s1 += c.String()
		}
		return s1
	}
	t.Run("subscribers muted", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{
			TrackInfo: &livekit.TrackInfo{
				Sid:    "v1",
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
			},
		})
		mt.Start()
		var lock sync.Mutex
		actualTrackID := livekit.TrackID("")
		actualSubscribedQualities := make([]*livekit.SubscribedCodec, 0)
		mt.OnSubscribedMaxQualityChange(func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedCodec, _maxSubscribedQualities []types.SubscribedCodecQuality) error {
			lock.Lock()
			actualTrackID = trackID
			actualSubscribedQualities = subscribedQualities
			lock.Unlock()
			return nil
		})

		mt.AddCodec(webrtc.MimeTypeVP8)
		mt.AddCodec(webrtc.MimeTypeAV1)

		mt.DynacastQuality.NotifySubscriberMaxQuality("s1", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_HIGH)
		mt.DynacastQuality.NotifySubscriberMaxQuality("s2", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1}, livekit.VideoQuality_HIGH)

		// mute all subscribers of vp8
		mt.DynacastQuality.NotifySubscriberMaxQuality("s1", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_OFF)

		expectedSubscribedQualities := []*livekit.SubscribedCodec{
			{
				Codec: webrtc.MimeTypeVP8,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: webrtc.MimeTypeAV1,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: true},
				},
			},
		}
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return livekit.TrackID("v1") == actualTrackID &&
				subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)
	})

	t.Run("subscribers max quality", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{
			TrackInfo: &livekit.TrackInfo{
				Sid:    "v1",
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
			},
			VideoConfig: config.VideoConfig{
				DynacastPauseDelay: 100 * time.Millisecond,
			},
		})
		mt.Start()

		mt.AddCodec(webrtc.MimeTypeVP8)
		mt.AddCodec(webrtc.MimeTypeAV1)

		lock := sync.RWMutex{}
		lock.Lock()
		actualTrackID := livekit.TrackID("")
		actualSubscribedQualities := make([]*livekit.SubscribedCodec, 0)
		lock.Unlock()
		mt.OnSubscribedMaxQualityChange(func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedCodec, _maxSubscribedQualities []types.SubscribedCodecQuality) error {
			lock.Lock()
			actualTrackID = trackID
			actualSubscribedQualities = subscribedQualities
			lock.Unlock()
			return nil
		})

		mt.maxSubscribedQuality = map[string]livekit.VideoQuality{
			webrtc.MimeTypeVP8: livekit.VideoQuality_LOW,
			webrtc.MimeTypeAV1: livekit.VideoQuality_LOW,
		}
		mt.DynacastQuality.NotifySubscriberMaxQuality("s1", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_HIGH)
		mt.DynacastQuality.NotifySubscriberMaxQuality("s2", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_MEDIUM)
		mt.DynacastQuality.NotifySubscriberMaxQuality("s3", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1}, livekit.VideoQuality_MEDIUM)

		expectedSubscribedQualities := []*livekit.SubscribedCodec{
			{
				Codec: webrtc.MimeTypeVP8,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: true},
				},
			},
			{
				Codec: webrtc.MimeTypeAV1,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
		}
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return livekit.TrackID("v1") == actualTrackID &&
				subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// "s1" dropping to MEDIUM should disable HIGH layer
		mt.DynacastQuality.NotifySubscriberMaxQuality("s1", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_MEDIUM)

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: webrtc.MimeTypeVP8,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: webrtc.MimeTypeAV1,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
		}
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return livekit.TrackID("v1") == actualTrackID &&
				subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// "s1" , "s2" , "s3" dropping to LOW should disable HIGH & MEDIUM
		mt.DynacastQuality.NotifySubscriberMaxQuality("s1", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_LOW)
		mt.DynacastQuality.NotifySubscriberMaxQuality("s2", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_LOW)
		mt.DynacastQuality.NotifySubscriberMaxQuality("s3", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1}, livekit.VideoQuality_LOW)

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: webrtc.MimeTypeVP8,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: webrtc.MimeTypeAV1,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
		}
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return livekit.TrackID("v1") == actualTrackID &&
				subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// muting "s2" only should not disable all qualities of vp8, no change of expected qualities
		mt.DynacastQuality.NotifySubscriberMaxQuality("s2", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_OFF)

		time.Sleep(100 * time.Millisecond)
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return livekit.TrackID("v1") == actualTrackID &&
				subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// muting "s1" and s3 also should disable all qualities
		mt.DynacastQuality.NotifySubscriberMaxQuality("s1", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_OFF)
		mt.DynacastQuality.NotifySubscriberMaxQuality("s3", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1}, livekit.VideoQuality_OFF)

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: webrtc.MimeTypeVP8,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: webrtc.MimeTypeAV1,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
		}
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return livekit.TrackID("v1") == actualTrackID &&
				subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// unmuting "s1" should enable vp8 previously set max quality
		mt.DynacastQuality.NotifySubscriberMaxQuality("s1", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, livekit.VideoQuality_LOW)

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: webrtc.MimeTypeVP8,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: webrtc.MimeTypeAV1,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
		}
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return livekit.TrackID("v1") == actualTrackID &&
				subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// a higher quality from a different node should trigger that quality
		mt.DynacastQuality.NotifySubscriberNodeMaxQuality("n1", []types.SubscribedCodecQuality{
			{CodecMime: webrtc.MimeTypeVP8, Quality: livekit.VideoQuality_HIGH},
			{CodecMime: webrtc.MimeTypeAV1, Quality: livekit.VideoQuality_MEDIUM},
		})

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: webrtc.MimeTypeVP8,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: true},
				},
			},
			{
				Codec: webrtc.MimeTypeAV1,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
		}
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return livekit.TrackID("v1") == actualTrackID &&
				subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)
	})
}
