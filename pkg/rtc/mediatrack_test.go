package rtc

import (
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
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
	t.Run("subscribers muted", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{TrackInfo: &livekit.TrackInfo{
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
		}})

		mt.notifySubscriberMaxQuality("s1", livekit.VideoQuality_HIGH)

		actualTrackID := livekit.TrackID("")
		actualSubscribedQualities := make([]*livekit.SubscribedQuality, 0)
		mt.OnSubscribedMaxQualityChange(func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality, _maxSubscribedQuality livekit.VideoQuality) error {
			actualTrackID = trackID
			actualSubscribedQualities = subscribedQualities
			return nil
		})

		// mute all subscribers
		mt.notifySubscriberMaxQuality("s1", livekit.VideoQuality_OFF)

		expectedSubscribedQualities := []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: false},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
			{Quality: livekit.VideoQuality_HIGH, Enabled: false},
		}
		require.Equal(t, livekit.TrackID("v1"), actualTrackID)
		require.EqualValues(t, expectedSubscribedQualities, actualSubscribedQualities)
	})

	t.Run("subscribers max quality", func(t *testing.T) {
		mt := NewMediaTrack(MediaTrackParams{TrackInfo: &livekit.TrackInfo{
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
		}})

		actualTrackID := livekit.TrackID("")
		actualSubscribedQualities := make([]*livekit.SubscribedQuality, 0)
		mt.OnSubscribedMaxQualityChange(func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality, _maxSubscribedQuality livekit.VideoQuality) error {
			actualTrackID = trackID
			actualSubscribedQualities = subscribedQualities
			return nil
		})

		mt.notifySubscriberMaxQuality("s1", livekit.VideoQuality_HIGH)
		mt.notifySubscriberMaxQuality("s2", livekit.VideoQuality_MEDIUM)

		expectedSubscribedQualities := []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: true},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
			{Quality: livekit.VideoQuality_HIGH, Enabled: true},
		}
		require.Equal(t, livekit.TrackID("v1"), actualTrackID)
		require.EqualValues(t, expectedSubscribedQualities, actualSubscribedQualities)

		// "s1" dropping to MEDIUM should disable HIGH layer
		mt.notifySubscriberMaxQuality("s1", livekit.VideoQuality_MEDIUM)

		expectedSubscribedQualities = []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: true},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
			{Quality: livekit.VideoQuality_HIGH, Enabled: false},
		}
		require.Equal(t, livekit.TrackID("v1"), actualTrackID)
		require.EqualValues(t, expectedSubscribedQualities, actualSubscribedQualities)

		// "s1" and "s1" dropping to LOW should disable HIGH & MEDIUM
		mt.notifySubscriberMaxQuality("s1", livekit.VideoQuality_LOW)
		mt.notifySubscriberMaxQuality("s2", livekit.VideoQuality_LOW)

		expectedSubscribedQualities = []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: true},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
			{Quality: livekit.VideoQuality_HIGH, Enabled: false},
		}
		require.Equal(t, livekit.TrackID("v1"), actualTrackID)
		require.EqualValues(t, expectedSubscribedQualities, actualSubscribedQualities)

		// muting "s2" only should not disable all qualities
		mt.notifySubscriberMaxQuality("s2", livekit.VideoQuality_OFF)

		expectedSubscribedQualities = []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: true},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
			{Quality: livekit.VideoQuality_HIGH, Enabled: false},
		}
		require.Equal(t, livekit.TrackID("v1"), actualTrackID)
		require.EqualValues(t, expectedSubscribedQualities, actualSubscribedQualities)

		// muting "s1" also should disable all qualities
		mt.notifySubscriberMaxQuality("s1", livekit.VideoQuality_OFF)

		expectedSubscribedQualities = []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: false},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
			{Quality: livekit.VideoQuality_HIGH, Enabled: false},
		}
		require.Equal(t, livekit.TrackID("v1"), actualTrackID)
		require.EqualValues(t, expectedSubscribedQualities, actualSubscribedQualities)

		// unmuting "s1" should enable previously set max quality
		mt.notifySubscriberMaxQuality("s1", livekit.VideoQuality_LOW)

		expectedSubscribedQualities = []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: true},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
			{Quality: livekit.VideoQuality_HIGH, Enabled: false},
		}
		require.Equal(t, livekit.TrackID("v1"), actualTrackID)
		require.EqualValues(t, expectedSubscribedQualities, actualSubscribedQualities)

		// a higher quality from a different node should trigger that quality
		mt.NotifySubscriberNodeMaxQuality("n1", livekit.VideoQuality_HIGH)

		expectedSubscribedQualities = []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: true},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
			{Quality: livekit.VideoQuality_HIGH, Enabled: true},
		}
		require.Equal(t, livekit.TrackID("v1"), actualTrackID)
		require.EqualValues(t, expectedSubscribedQualities, actualSubscribedQualities)
	})
}
