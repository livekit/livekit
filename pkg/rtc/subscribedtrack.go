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
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	subscriptionDebounceInterval = 100 * time.Millisecond
)

type SubscribedTrackParams struct {
	PublisherID       livekit.ParticipantID
	PublisherIdentity livekit.ParticipantIdentity
	PublisherVersion  uint32
	Subscriber        types.LocalParticipant
	MediaTrack        types.MediaTrack
	DownTrack         *sfu.DownTrack
	AdaptiveStream    bool
}

type SubscribedTrack struct {
	params           SubscribedTrackParams
	logger           logger.Logger
	sender           atomic.Pointer[webrtc.RTPSender]
	needsNegotiation atomic.Bool

	versionGenerator utils.TimedVersionGenerator
	settingsLock     sync.Mutex
	settings         *livekit.UpdateTrackSettings
	settingsVersion  utils.TimedVersion

	bindLock        sync.Mutex
	bound           bool
	onBindCallbacks []func(error)

	onClose atomic.Value // func(bool)

	debouncer func(func())
}

func NewSubscribedTrack(params SubscribedTrackParams) *SubscribedTrack {
	s := &SubscribedTrack{
		params: params,
		logger: params.Subscriber.GetLogger().WithComponent(sutils.ComponentSub).WithValues(
			"trackID", params.DownTrack.ID(),
			"publisherID", params.PublisherID,
			"publisher", params.PublisherIdentity,
		),
		versionGenerator: utils.NewDefaultTimedVersionGenerator(),
		debouncer:        debounce.New(subscriptionDebounceInterval),
	}

	return s
}

func (t *SubscribedTrack) AddOnBind(f func(error)) {
	t.bindLock.Lock()
	bound := t.bound
	if !bound {
		t.onBindCallbacks = append(t.onBindCallbacks, f)
	}
	t.bindLock.Unlock()

	if bound {
		// fire immediately, do not need to persist since bind is a one time event
		go f(nil)
	}
}

// for DownTrack callback to notify us that it's bound
func (t *SubscribedTrack) Bound(err error) {
	t.bindLock.Lock()
	if err == nil {
		t.bound = true
	}
	callbacks := t.onBindCallbacks
	t.onBindCallbacks = nil
	t.bindLock.Unlock()

	if err == nil && t.MediaTrack().Kind() == livekit.TrackType_VIDEO {
		// When AdaptiveStream is enabled, default the subscriber to LOW quality stream
		// we would want LOW instead of OFF for a couple of reasons
		// 1. when a subscriber unsubscribes from a track, we would forget their previously defined settings
		//    depending on client implementation, subscription on/off is kept separately from adaptive stream
		//    So when there are no changes to desired resolution, but the user re-subscribes, we may leave stream at OFF
		// 2. when interacting with dynacast *and* adaptive stream. If the publisher was not publishing at the
		//    time of subscription, we might not be able to trigger adaptive stream updates on the client side
		//    (since there isn't any video frames coming through). this will leave the stream "stuck" on off, without
		//    a trigger to re-enable it
		t.settingsLock.Lock()
		if t.settings != nil {
			if t.params.AdaptiveStream {
				// remove `disabled` flag to force a visibility update
				t.settings.Disabled = false
			}
		} else {
			if t.params.AdaptiveStream {
				t.settings = &livekit.UpdateTrackSettings{Quality: livekit.VideoQuality_LOW}
			} else {
				t.settings = &livekit.UpdateTrackSettings{Quality: livekit.VideoQuality_HIGH}
			}
		}
		t.settingsLock.Unlock()
		t.applySettings()
	}

	for _, cb := range callbacks {
		go cb(err)
	}
}

// for DownTrack callback to notify us that it's closed
func (t *SubscribedTrack) Close(isExpectedToResume bool) {
	if onClose := t.onClose.Load(); onClose != nil {
		go onClose.(func(bool))(isExpectedToResume)
	}
}

func (t *SubscribedTrack) OnClose(f func(bool)) {
	t.onClose.Store(f)
}

func (t *SubscribedTrack) IsBound() bool {
	t.bindLock.Lock()
	defer t.bindLock.Unlock()

	return t.bound
}

func (t *SubscribedTrack) ID() livekit.TrackID {
	return livekit.TrackID(t.params.DownTrack.ID())
}

func (t *SubscribedTrack) PublisherID() livekit.ParticipantID {
	return t.params.PublisherID
}

func (t *SubscribedTrack) PublisherIdentity() livekit.ParticipantIdentity {
	return t.params.PublisherIdentity
}

func (t *SubscribedTrack) PublisherVersion() uint32 {
	return t.params.PublisherVersion
}

func (t *SubscribedTrack) SubscriberID() livekit.ParticipantID {
	return t.params.Subscriber.ID()
}

func (t *SubscribedTrack) SubscriberIdentity() livekit.ParticipantIdentity {
	return t.params.Subscriber.Identity()
}

func (t *SubscribedTrack) Subscriber() types.LocalParticipant {
	return t.params.Subscriber
}

func (t *SubscribedTrack) DownTrack() *sfu.DownTrack {
	return t.params.DownTrack
}

func (t *SubscribedTrack) MediaTrack() types.MediaTrack {
	return t.params.MediaTrack
}

// has subscriber indicated it wants to mute this track
func (t *SubscribedTrack) IsMuted() bool {
	t.settingsLock.Lock()
	defer t.settingsLock.Unlock()

	return t.isMutedLocked()
}

func (t *SubscribedTrack) isMutedLocked() bool {
	if t.settings == nil {
		return false
	}

	return t.settings.Disabled
}

func (t *SubscribedTrack) SetPublisherMuted(muted bool) {
	t.DownTrack().PubMute(muted)
}

func (t *SubscribedTrack) UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings, isImmediate bool) {
	t.settingsLock.Lock()
	if proto.Equal(t.settings, settings) {
		t.settingsLock.Unlock()
		return
	}

	isImmediate = isImmediate || (!settings.Disabled && settings.Disabled != t.isMutedLocked())
	t.settings = utils.CloneProto(settings)
	t.settingsLock.Unlock()

	if isImmediate {
		t.applySettings()
	} else {
		// avoid frequent changes to mute & video layers, unless it became visible
		t.debouncer(t.applySettings)
	}
}

func (t *SubscribedTrack) UpdateVideoLayer() {
	t.applySettings()
}

func (t *SubscribedTrack) applySettings() {
	t.settingsLock.Lock()
	if t.settings == nil {
		t.settingsLock.Unlock()
		return
	}

	t.logger.Debugw("updating subscriber track settings", "settings", logger.Proto(t.settings))
	t.settingsVersion = t.versionGenerator.Next()
	settingsVersion := t.settingsVersion
	t.settingsLock.Unlock()

	dt := t.DownTrack()
	spatial := buffer.InvalidLayerSpatial
	temporal := buffer.InvalidLayerTemporal
	if dt.Kind() == webrtc.RTPCodecTypeVideo {
		mt := t.MediaTrack()
		quality := t.settings.Quality
		if t.settings.Width > 0 {
			quality = mt.GetQualityForDimension(t.settings.Width, t.settings.Height)
		}

		spatial = buffer.VideoQualityToSpatialLayer(quality, mt.ToProto())
		if t.settings.Fps > 0 {
			temporal = mt.GetTemporalLayerForSpatialFps(spatial, t.settings.Fps, dt.Codec().MimeType)
		}
	}

	t.settingsLock.Lock()
	if settingsVersion != t.settingsVersion {
		// a newer settings has superceded this one
		t.settingsLock.Unlock()
		return
	}

	if t.settings.Disabled {
		dt.Mute(true)
		t.settingsLock.Unlock()
		return
	} else {
		dt.Mute(false)
	}

	if dt.Kind() == webrtc.RTPCodecTypeVideo {
		dt.SetMaxSpatialLayer(spatial)
		if temporal != buffer.InvalidLayerTemporal {
			dt.SetMaxTemporalLayer(temporal)
		}
	}
	t.settingsLock.Unlock()
}

func (t *SubscribedTrack) NeedsNegotiation() bool {
	return t.needsNegotiation.Load()
}

func (t *SubscribedTrack) SetNeedsNegotiation(needs bool) {
	t.needsNegotiation.Store(needs)
}

func (t *SubscribedTrack) RTPSender() *webrtc.RTPSender {
	return t.sender.Load()
}

func (t *SubscribedTrack) SetRTPSender(sender *webrtc.RTPSender) {
	t.sender.Store(sender)
}
