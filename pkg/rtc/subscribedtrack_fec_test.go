// Copyright 2026 LiveKit, Inc.
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
	"sync"
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
)

func fecDisabled() *livekit.FECProtection {
	return &livekit.FECProtection{}
}

func fecFixed(level livekit.FECProtectionLevel) *livekit.FECProtection {
	return &livekit.FECProtection{Mode: livekit.FECProtectionMode_FEC_MODE_FIXED, Level: level}
}

func TestSubscriberFECSettings(t *testing.T) {
	require.Nil(t, mergeSubscriberSettings(nil, &livekit.UpdateTrackSettings{}).Fec)
	for _, tc := range []struct {
		requested, want *livekit.FECProtection
	}{
		{fecDisabled(), fecDisabled()},
		{fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_LOW), fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_LOW)},
		{fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_MEDIUM), fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_MEDIUM)},
		{fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_HIGH), fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_HIGH)},
		{
			&livekit.FECProtection{Mode: livekit.FECProtectionMode_FEC_MODE_ADAPTIVE},
			&livekit.FECProtection{Mode: livekit.FECProtectionMode_FEC_MODE_ADAPTIVE},
		},
		{
			&livekit.FECProtection{Mode: livekit.FECProtectionMode_FEC_MODE_MINIMUM, Level: livekit.FECProtectionLevel_FEC_LEVEL_HIGH},
			&livekit.FECProtection{Mode: livekit.FECProtectionMode_FEC_MODE_MINIMUM, Level: livekit.FECProtectionLevel_FEC_LEVEL_HIGH},
		},
		// unknown modes and levels are kept as sent, the DownTrack interprets them
		{&livekit.FECProtection{Mode: -1, Level: livekit.FECProtectionLevel_FEC_LEVEL_LOW}, &livekit.FECProtection{Mode: -1, Level: livekit.FECProtectionLevel_FEC_LEVEL_LOW}},
		{&livekit.FECProtection{Mode: 99}, &livekit.FECProtection{Mode: 99}},
		{fecFixed(99), fecFixed(99)},
	} {
		requested := &livekit.UpdateTrackSettings{Fec: tc.requested}
		original := utils.CloneProto(tc.requested)
		cached := mergeSubscriberSettings(nil, requested)
		require.NotNil(t, cached.Fec)
		require.True(t, proto.Equal(tc.want, cached.Fec), "got %v, want %v", cached.Fec, tc.want)
		require.True(t, proto.Equal(original, requested.Fec), "do not change caller-owned messages")
		legacyUpdate := &livekit.UpdateTrackSettings{Disabled: true, Width: 640}
		merged := mergeSubscriberSettings(cached, legacyUpdate)
		require.True(t, merged.Disabled)
		require.EqualValues(t, 640, merged.Width)
		require.True(t, proto.Equal(cached.Fec, merged.Fec))
		require.Nil(t, legacyUpdate.Fec)
		merged.Fec.Level = 42
		require.True(t, proto.Equal(tc.want, cached.Fec), "cached option must not alias later settings")
	}
}

func TestSubscriberFECPersistsAcrossResubscription(t *testing.T) {
	sub := newMediaTrackSubscription("subscriber", "track", logger.GetLogger())
	sub.setSettings(&livekit.UpdateTrackSettings{Fec: fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_MEDIUM)})
	first := &typesfakes.FakeSubscribedTrack{}
	sub.setSubscribedTrack(first)
	got, immediate := first.UpdateSubscriberSettingsArgsForCall(0)
	require.True(t, proto.Equal(fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_MEDIUM), got.Fec))
	require.True(t, immediate)
	sub.setSettings(&livekit.UpdateTrackSettings{Width: 640, Height: 360})
	got, immediate = first.UpdateSubscriberSettingsArgsForCall(1)
	require.True(t, proto.Equal(fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_MEDIUM), got.Fec))
	require.False(t, immediate)
	sub.setSettings(&livekit.UpdateTrackSettings{Fec: fecDisabled()})
	sub.setSettings(&livekit.UpdateTrackSettings{Quality: livekit.VideoQuality_HIGH})
	sub.setSubscribedTrack(nil)
	second := &typesfakes.FakeSubscribedTrack{}
	sub.setSubscribedTrack(second)
	got, immediate = second.UpdateSubscriberSettingsArgsForCall(0)
	require.NotNil(t, got.Fec)
	require.True(t, proto.Equal(fecDisabled(), got.Fec), "explicit off must survive resubscription and quality updates")
	require.True(t, immediate)
}

func TestSubscriberFECAppliesToDownTrack(t *testing.T) {
	sub, _ := newSubscriberFECTestTrack(t)
	dt := sub.DownTrack()
	for _, tc := range []struct {
		level *livekit.FECProtection
		want  int64
	}{
		{nil, 100_000},
		{fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_MEDIUM), 125_000},
		{nil, 125_000},
		{fecDisabled(), 100_000},
		{nil, 100_000},
		{fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_LOW), 115_000},
		{fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_HIGH), 135_000},
		{&livekit.FECProtection{Mode: 99, Level: livekit.FECProtectionLevel_FEC_LEVEL_HIGH}, 100_000},
		{&livekit.FECProtection{Mode: livekit.FECProtectionMode_FEC_MODE_MINIMUM}, 115_000},
		{nil, 115_000},
		{&livekit.FECProtection{Mode: livekit.FECProtectionMode_FEC_MODE_ADAPTIVE}, 100_000},
	} {
		sub.UpdateSubscriberSettings(&livekit.UpdateTrackSettings{Quality: livekit.VideoQuality_HIGH, Fec: tc.level}, true)
		dt.AllocateOptimal(false, false)
		require.Equal(t, tc.want, dt.BandwidthRequested(), "client settings must reach the negotiated encoder and allocator")
	}
}

func TestSubscriberFECSettingsSupersededDuringApply(t *testing.T) {
	sub, mediaTrack := newSubscriberFECTestTrack(t)
	sub.UpdateSubscriberSettings(&livekit.UpdateTrackSettings{Quality: livekit.VideoQuality_HIGH}, true)
	var pending func()
	sub.debouncer = func(f func()) { pending = f }
	mediaTrack.GetQualityForDimensionCalls(func(mime.MimeType, uint32, uint32) livekit.VideoQuality {
		// A settings update can arrive while layer selection runs without settingsLock.
		sub.UpdateSubscriberSettings(&livekit.UpdateTrackSettings{
			Quality: livekit.VideoQuality_HIGH,
			Fec:     fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_HIGH),
		}, false)
		return livekit.VideoQuality_HIGH
	})
	sub.UpdateSubscriberSettings(&livekit.UpdateTrackSettings{
		Width: 640,
		Fec:   fecFixed(livekit.FECProtectionLevel_FEC_LEVEL_MEDIUM),
	}, true)
	dt := sub.DownTrack()
	dt.AllocateOptimal(false, false)
	require.EqualValues(t, 100_000, dt.BandwidthRequested(), "superseded settings must not apply part of a newer update")
	require.NotNil(t, pending)
	pending()
	dt.AllocateOptimal(false, false)
	require.EqualValues(t, 135_000, dt.BandwidthRequested())
}

func TestSubscriberFECConcurrentSettings(t *testing.T) {
	sub, _ := newSubscriberFECTestTrack(t)
	var wg sync.WaitGroup
	for level := range 4 {
		wg.Go(func() {
			for range 100 {
				sub.UpdateSubscriberSettings(&livekit.UpdateTrackSettings{
					Width: 640,
					Fec:   &livekit.FECProtection{Mode: livekit.FECProtectionMode(level), Level: livekit.FECProtectionLevel(level)},
				}, true)
			}
		})
	}
	wg.Wait()
}

func newSubscriberFECTestTrack(t *testing.T) (*SubscribedTrack, *typesfakes.FakeMediaTrack) {
	t.Helper()
	codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		PayloadType:        96,
	}
	receiver := &sfufakes.FakeTrackReceiver{}
	receiver.TrackIDReturns("track")
	receiver.CodecReturns(codec)
	receiver.AddOnReadyCalls(func(f func()) { f() })
	receiver.GetLayeredBitrateReturns([]int32{0}, sfu.Bitrates{{100_000}})
	dt, err := sfu.NewDownTrack(sfu.DownTrackParams{
		EnableFlexFEC: true,
		Codecs:        []webrtc.RTPCodecParameters{codec},
		Receiver:      receiver,
		BufferFactory: buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory(),
		MaxTrack:      500,
		Logger:        logger.GetLogger(),
		Listener:      &sfufakes.FakeDownTrackListener{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { dt.CloseWithFlush(false, true) })
	_, err = dt.Bind(subscriberFECTrackContext{codecs: []webrtc.RTPCodecParameters{
		codec,
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeFlexFEC03, ClockRate: 90000}, PayloadType: 115},
	}})
	require.NoError(t, err)
	dt.UpTrackMaxPublishedLayerChange(0)
	dt.UpTrackMaxTemporalLayerSeenChange(0)
	mediaTrack := &typesfakes.FakeMediaTrack{}
	mediaTrack.ToProtoReturns(&livekit.TrackInfo{Type: livekit.TrackType_VIDEO})
	sub := &SubscribedTrack{
		params:           SubscribedTrackParams{MediaTrack: mediaTrack},
		downTrack:        dt,
		logger:           logger.GetLogger(),
		versionGenerator: utils.NewDefaultTimedVersionGenerator(),
	}
	return sub, mediaTrack
}

type subscriberFECTrackContext struct {
	webrtc.TrackLocalContext
	codecs []webrtc.RTPCodecParameters
}

func (c subscriberFECTrackContext) CodecParameters() []webrtc.RTPCodecParameters { return c.codecs }
func (subscriberFECTrackContext) SSRC() webrtc.SSRC                              { return 123 }
func (subscriberFECTrackContext) SSRCRetransmission() webrtc.SSRC                { return 0 }
func (subscriberFECTrackContext) SSRCForwardErrorCorrection() webrtc.SSRC        { return 456 }
func (subscriberFECTrackContext) WriteStream() webrtc.TrackLocalWriter           { return nil }
