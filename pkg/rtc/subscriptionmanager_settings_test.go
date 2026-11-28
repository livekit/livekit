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
	"fmt"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

type settingsRestoreGate struct {
	types.SubscribedTrack
	entered chan struct{}
	release chan struct{}
}

func (s *settingsRestoreGate) UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings, immediate bool) {
	if immediate {
		close(s.entered)
		<-s.release
	}
	s.SubscribedTrack.UpdateSubscriberSettings(settings, immediate)
}

func TestSettingsDuringSubscription(t *testing.T) {
	for _, kind := range []livekit.TrackType{livekit.TrackType_AUDIO, livekit.TrackType_VIDEO} {
		for _, oldMuted := range []bool{false, true} {
			for _, newMuted := range []bool{false, true} {
				for _, order := range []string{"before", "during", "after"} {
					t.Run(fmt.Sprintf("%s/%t_to_%t/%s", kind, oldMuted, newMuted, order), func(t *testing.T) {
						testSubscriptionSettingsHandoff(t, kind, order,
							&livekit.UpdateTrackSettings{Disabled: oldMuted, Quality: livekit.VideoQuality_HIGH, Fps: 30},
							&livekit.UpdateTrackSettings{Disabled: newMuted, Quality: livekit.VideoQuality_LOW, Fps: 15},
						)
					})
				}
			}
		}
	}
	for _, order := range []string{"before", "during", "after"} {
		t.Run("dimensions/"+order, func(t *testing.T) {
			testSubscriptionSettingsHandoff(t, livekit.TrackType_VIDEO, order,
				&livekit.UpdateTrackSettings{Width: 1280, Height: 720, Fps: 30},
				&livekit.UpdateTrackSettings{Width: 320, Height: 180, Fps: 15},
			)
		})
	}
}

func testSubscriptionSettingsHandoff(t *testing.T, kind livekit.TrackType, order string, old, next *livekit.UpdateTrackSettings) {
	t.Helper()
	const id livekit.TrackID = "track"
	sm := newTestSubscriptionManager()
	// Drive installation directly so the restore can be paused at the handoff.
	close(sm.closeCh)
	<-sm.doneCh
	sm.closeCh = make(chan struct{})
	p := sm.params.Participant.(*typesfakes.FakeLocalParticipant)
	p.GetLoggerReturns(logger.GetLogger())
	p.GetReporterReturns(NewMockParticipant("sub", types.CurrentProtocol, false, false, nil).GetReporter())
	mt := NewMockTrack(kind, "published")
	mt.IDReturns(id)
	mt.PublisherIDReturns("pubID")
	mt.IsOpenReturns(true)
	mt.ToProtoReturns(&livekit.TrackInfo{Type: kind, Layers: []*livekit.VideoLayer{
		{Quality: livekit.VideoQuality_LOW, SpatialLayer: 0, Width: 320, Height: 180},
		{Quality: livekit.VideoQuality_HIGH, SpatialLayer: 2, Width: 1280, Height: 720},
	}})
	mt.GetQualityForDimensionCalls(func(_ mime.MimeType, width, height uint32) livekit.VideoQuality {
		if width <= 320 && height <= 180 {
			return livekit.VideoQuality_LOW
		}
		return livekit.VideoQuality_HIGH
	})
	mt.GetTemporalLayerForSpatialFpsCalls(func(_ mime.MimeType, _ int32, fps uint32) int32 {
		if fps <= 15 {
			return 0
		}
		return 2
	})
	pub := NewMediaTrackSubscriptions(MediaTrackSubscriptionsParams{MediaTrack: mt, Logger: logger.GetLogger()})
	codec := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, PayloadType: 111}
	if kind == livekit.TrackType_VIDEO {
		codec = webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, PayloadType: 96}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var track *SubscribedTrack
	mt.AddSubscriberCalls(func(sub types.LocalParticipant) (types.SubscribedTrack, error) {
		wr := NewWrappedReceiver(WrappedReceiverParams{TrackID: id, UpstreamCodecs: []webrtc.RTPCodecParameters{codec}, Logger: logger.GetLogger(), DisableRed: true})
		st, err := pub.AddSubscriber(sub, wr)
		if err != nil {
			return st, err
		}
		track = st.(*SubscribedTrack)
		// Test request ordering without waiting for the video-layer debounce timer.
		track.debouncer = func(fn func()) { fn() }
		if order == "during" {
			return &settingsRestoreGate{SubscribedTrack: st, entered: entered, release: release}, nil
		}
		return st, nil
	})
	t.Cleanup(func() {
		if track != nil {
			track.DownTrack().CloseWithFlush(true, true)
		}
		sm.Close(false)
	})
	sm.params.TrackResolver = func(types.LocalParticipant, livekit.TrackID) types.MediaResolverResult {
		return types.MediaResolverResult{Track: mt, HasPermission: true, PublisherID: "pubID"}
	}
	wait := func(done <-chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("subscription operation did not finish")
		}
	}
	sm.UpdateSubscribedTrackSettings(id, old)
	if order == "before" {
		sm.UpdateSubscribedTrackSettings(id, next)
	}
	sm.SubscribeToTrack(id, false)
	if order == "during" {
		installed := make(chan struct{})
		go func() { defer close(installed); sm.reconcileSubscriptions() }()
		wait(entered)
		updated := make(chan struct{})
		go func() { defer close(updated); sm.UpdateSubscribedTrackSettings(id, next) }()
		// Give the live update a chance to overtake the paused restore. An ordered
		// implementation can instead hold it until restoration completes.
		select {
		case <-updated:
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		wait(installed)
		wait(updated)
	} else {
		sm.reconcileSubscriptions()
		if order == "after" {
			sm.UpdateSubscribedTrackSettings(id, next)
		}
	}

	for _, reapply := range []bool{false, true} {
		if reapply {
			track.UpdateVideoLayer()
		}
		sm.lock.RLock()
		sub := sm.subscriptions[id]
		sm.lock.RUnlock()
		sub.lock.RLock()
		saved := sub.settings
		sub.lock.RUnlock()
		track.settingsLock.Lock()
		applied := proto.CloneOf(track.settings)
		track.settingsLock.Unlock()
		muted := track.DownTrack().DebugInfo()["Muted"].(bool)
		if !proto.Equal(saved, next) || !proto.Equal(applied, next) || track.IsMuted() != next.Disabled || muted != next.Disabled {
			t.Errorf("latest subscriber settings lost (reapply=%t): saved=%v track=%v forwarding_muted=%t want=%v", reapply, saved, applied, muted, next)
		}
		if kind == livekit.TrackType_VIDEO && !next.Disabled {
			layer := track.DownTrack().MaxLayer()
			if layer.Spatial != 0 || layer.Temporal != 0 {
				t.Errorf("latest subscriber settings not forwarded (reapply=%t): layer=%v want spatial=0 temporal=0", reapply, layer)
			}
		}
	}
}
