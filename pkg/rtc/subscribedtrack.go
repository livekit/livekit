package rtc

import (
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

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
	subMuted         atomic.Bool
	pubMuted         atomic.Bool
	settings         atomic.Pointer[livekit.UpdateTrackSettings]
	logger           logger.Logger
	sender           atomic.Pointer[webrtc.RTPSender]
	needsNegotiation atomic.Bool

	bindLock        sync.Mutex
	onBindCallbacks []func(error)
	onClose         atomic.Value // func(bool)
	bound           atomic.Bool

	debouncer func(func())
}

func NewSubscribedTrack(params SubscribedTrackParams) *SubscribedTrack {
	s := &SubscribedTrack{
		params: params,
		logger: params.Subscriber.GetLogger().WithValues(
			"trackID", params.DownTrack.ID(),
			"publisherID", params.PublisherID,
			"publisher", params.PublisherIdentity,
		),
		debouncer: debounce.New(subscriptionDebounceInterval),
	}

	return s
}

func (t *SubscribedTrack) AddOnBind(f func(error)) {
	t.bindLock.Lock()
	bound := t.bound.Load()
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
		t.bound.Store(true)
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
		var desiredLayer int32
		if t.params.AdaptiveStream {
			desiredLayer = buffer.VideoQualityToSpatialLayer(livekit.VideoQuality_LOW, t.params.MediaTrack.ToProto())
		} else {
			desiredLayer = buffer.VideoQualityToSpatialLayer(livekit.VideoQuality_HIGH, t.params.MediaTrack.ToProto())
		}
		settings := t.settings.Load()
		if settings != nil {
			desiredLayer = t.spatialLayerFromSettings(settings)
		}
		t.DownTrack().SetMaxSpatialLayer(desiredLayer)
	}

	for _, cb := range callbacks {
		go cb(err)
	}
}

// for DownTrack callback to notify us that it's closed
func (t *SubscribedTrack) Close(willBeResumed bool) {
	if onClose := t.onClose.Load(); onClose != nil {
		go onClose.(func(bool))(willBeResumed)
	}
}

func (t *SubscribedTrack) OnClose(f func(bool)) {
	t.onClose.Store(f)
}

func (t *SubscribedTrack) IsBound() bool {
	return t.bound.Load()
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
	return t.subMuted.Load()
}

func (t *SubscribedTrack) SetPublisherMuted(muted bool) {
	t.pubMuted.Store(muted)
	t.updateDownTrackMute()
}

func (t *SubscribedTrack) UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings) {
	prevDisabled := t.subMuted.Swap(settings.Disabled)
	t.settings.Store(settings)

	if prevDisabled != settings.Disabled {
		t.logger.Infow("updated subscribed track enabled", "enabled", !settings.Disabled)
	}

	// avoid frequent changes to mute & video layers, unless it became visible
	if prevDisabled != settings.Disabled && !settings.Disabled {
		t.UpdateVideoLayer()
	} else {
		t.debouncer(t.UpdateVideoLayer)
	}
}

func (t *SubscribedTrack) UpdateVideoLayer() {
	t.updateDownTrackMute()
	if t.DownTrack().Kind() != webrtc.RTPCodecTypeVideo {
		return
	}

	settings := t.settings.Load()
	if settings == nil {
		return
	}

	t.logger.Debugw("updating video layer",
		"settings", settings,
	)

	spatial := t.spatialLayerFromSettings(settings)
	t.DownTrack().SetMaxSpatialLayer(spatial)
	if settings.Fps > 0 {
		t.DownTrack().SetMaxTemporalLayer(t.MediaTrack().GetTemporalLayerForSpatialFps(spatial, settings.Fps, t.DownTrack().Codec().MimeType))
	}
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

func (t *SubscribedTrack) updateDownTrackMute() {
	t.DownTrack().Mute(t.subMuted.Load())
	t.DownTrack().PubMute(t.pubMuted.Load())
}

func (t *SubscribedTrack) spatialLayerFromSettings(settings *livekit.UpdateTrackSettings) int32 {
	quality := settings.Quality
	if settings.Width > 0 {
		quality = t.MediaTrack().GetQualityForDimension(settings.Width, settings.Height)
	}

	return buffer.VideoQualityToSpatialLayer(quality, t.params.MediaTrack.ToProto())
}
