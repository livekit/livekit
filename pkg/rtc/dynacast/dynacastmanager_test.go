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

package dynacast

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
)

func TestSubscribedMaxQuality(t *testing.T) {

	t.Run("subscribers muted", func(t *testing.T) {
		dm := NewDynacastManager(DynacastManagerParams{})
		var lock sync.Mutex
		actualSubscribedQualities := make([]*livekit.SubscribedCodec, 0)
		dm.OnSubscribedMaxQualityChange(func(subscribedQualities []*livekit.SubscribedCodec, _maxSubscribedQualities []types.SubscribedCodecQuality) {
			lock.Lock()
			actualSubscribedQualities = subscribedQualities
			lock.Unlock()
		})

		dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeVP8, livekit.VideoQuality_HIGH)
		dm.NotifySubscriberMaxQuality("s2", mime.MimeTypeAV1, livekit.VideoQuality_HIGH)

		// mute all subscribers of vp8
		dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeVP8, livekit.VideoQuality_OFF)

		expectedSubscribedQualities := []*livekit.SubscribedCodec{
			{
				Codec: mime.MimeTypeVP8.String(),
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: mime.MimeTypeAV1.String(),
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

			return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)
	})

	t.Run("subscribers max quality", func(t *testing.T) {
		dm := NewDynacastManager(DynacastManagerParams{
			DynacastPauseDelay: 100 * time.Millisecond,
		})

		lock := sync.RWMutex{}
		lock.Lock()
		actualSubscribedQualities := make([]*livekit.SubscribedCodec, 0)
		lock.Unlock()
		dm.OnSubscribedMaxQualityChange(func(subscribedQualities []*livekit.SubscribedCodec, _maxSubscribedQualities []types.SubscribedCodecQuality) {
			lock.Lock()
			actualSubscribedQualities = subscribedQualities
			lock.Unlock()
		})

		dm.maxSubscribedQuality = map[mime.MimeType]livekit.VideoQuality{
			mime.MimeTypeVP8: livekit.VideoQuality_LOW,
			mime.MimeTypeAV1: livekit.VideoQuality_LOW,
		}
		dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeVP8, livekit.VideoQuality_HIGH)
		dm.NotifySubscriberMaxQuality("s2", mime.MimeTypeVP8, livekit.VideoQuality_MEDIUM)
		dm.NotifySubscriberMaxQuality("s3", mime.MimeTypeAV1, livekit.VideoQuality_MEDIUM)

		expectedSubscribedQualities := []*livekit.SubscribedCodec{
			{
				Codec: mime.MimeTypeVP8.String(),
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: true},
				},
			},
			{
				Codec: mime.MimeTypeAV1.String(),
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

			return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// "s1" dropping to MEDIUM should disable HIGH layer
		dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeVP8, livekit.VideoQuality_MEDIUM)

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: mime.MimeTypeVP8.String(),
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: mime.MimeTypeAV1.String(),
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

			return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// "s1" , "s2" , "s3" dropping to LOW should disable HIGH & MEDIUM
		dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeVP8, livekit.VideoQuality_LOW)
		dm.NotifySubscriberMaxQuality("s2", mime.MimeTypeVP8, livekit.VideoQuality_LOW)
		dm.NotifySubscriberMaxQuality("s3", mime.MimeTypeAV1, livekit.VideoQuality_LOW)

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: mime.MimeTypeVP8.String(),
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: mime.MimeTypeAV1.String(),
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

			return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// muting "s2" only should not disable all qualities of vp8, no change of expected qualities
		dm.NotifySubscriberMaxQuality("s2", mime.MimeTypeVP8, livekit.VideoQuality_OFF)

		time.Sleep(100 * time.Millisecond)
		require.Eventually(t, func() bool {
			lock.Lock()
			defer lock.Unlock()

			return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// muting "s1" and s3 also should disable all qualities
		dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeVP8, livekit.VideoQuality_OFF)
		dm.NotifySubscriberMaxQuality("s3", mime.MimeTypeAV1, livekit.VideoQuality_OFF)

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: mime.MimeTypeVP8.String(),
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: mime.MimeTypeAV1.String(),
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

			return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// unmuting "s1" should enable vp8 previously set max quality
		dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeVP8, livekit.VideoQuality_LOW)

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: mime.MimeTypeVP8.String(),
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			},
			{
				Codec: mime.MimeTypeAV1.String(),
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

			return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)

		// a higher quality from a different node should trigger that quality
		dm.NotifySubscriberNodeMaxQuality("n1", []types.SubscribedCodecQuality{
			{CodecMime: mime.MimeTypeVP8, Quality: livekit.VideoQuality_HIGH},
			{CodecMime: mime.MimeTypeAV1, Quality: livekit.VideoQuality_MEDIUM},
		})

		expectedSubscribedQualities = []*livekit.SubscribedCodec{
			{
				Codec: mime.MimeTypeVP8.String(),
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: true},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: true},
					{Quality: livekit.VideoQuality_HIGH, Enabled: true},
				},
			},
			{
				Codec: mime.MimeTypeAV1.String(),
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

			return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
		}, 10*time.Second, 100*time.Millisecond)
	})
}

func TestCodecRegression(t *testing.T) {
	dm := NewDynacastManager(DynacastManagerParams{})
	var lock sync.Mutex
	actualSubscribedQualities := make([]*livekit.SubscribedCodec, 0)
	dm.OnSubscribedMaxQualityChange(func(subscribedQualities []*livekit.SubscribedCodec, _maxSubscribedQualities []types.SubscribedCodecQuality) {
		lock.Lock()
		actualSubscribedQualities = subscribedQualities
		lock.Unlock()
	})

	dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeAV1, livekit.VideoQuality_HIGH)

	expectedSubscribedQualities := []*livekit.SubscribedCodec{
		{
			Codec: mime.MimeTypeAV1.String(),
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

		return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
	}, 10*time.Second, 100*time.Millisecond)

	dm.HandleCodecRegression(mime.MimeTypeAV1, mime.MimeTypeVP8)

	expectedSubscribedQualities = []*livekit.SubscribedCodec{
		{
			Codec: mime.MimeTypeAV1.String(),
			Qualities: []*livekit.SubscribedQuality{
				{Quality: livekit.VideoQuality_LOW, Enabled: false},
				{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
				{Quality: livekit.VideoQuality_HIGH, Enabled: false},
			},
		},
		{
			Codec: mime.MimeTypeVP8.String(),
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

		return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
	}, 10*time.Second, 100*time.Millisecond)

	// av1 quality change should be forwarded to vp8
	// av1 quality change of node should be ignored
	dm.NotifySubscriberMaxQuality("s1", mime.MimeTypeAV1, livekit.VideoQuality_MEDIUM)
	dm.NotifySubscriberNodeMaxQuality("n1", []types.SubscribedCodecQuality{
		{CodecMime: mime.MimeTypeAV1, Quality: livekit.VideoQuality_HIGH},
	})
	expectedSubscribedQualities = []*livekit.SubscribedCodec{
		{
			Codec: mime.MimeTypeAV1.String(),
			Qualities: []*livekit.SubscribedQuality{
				{Quality: livekit.VideoQuality_LOW, Enabled: false},
				{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
				{Quality: livekit.VideoQuality_HIGH, Enabled: false},
			},
		},
		{
			Codec: mime.MimeTypeVP8.String(),
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

		return subscribedCodecsAsString(expectedSubscribedQualities) == subscribedCodecsAsString(actualSubscribedQualities)
	}, 10*time.Second, 100*time.Millisecond)
}

func subscribedCodecsAsString(c1 []*livekit.SubscribedCodec) string {
	sort.Slice(c1, func(i, j int) bool { return c1[i].Codec < c1[j].Codec })
	var s1 string
	for _, c := range c1 {
		s1 += c.String()
	}
	return s1
}
