package rtc

import (
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

	onBind  atomic.Value // func()
	onClose atomic.Value // func(bool)
	bound   atomic.Bool

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

func (t *SubscribedTrack) OnBind(f func()) {
	t.onBind.Store(f)

	t.maybeOnBind()
}

// for DownTrack callback to notify us that it's bound
func (t *SubscribedTrack) Bound() {
	t.bound.Store(true)
	if !t.params.AdaptiveStream {
		t.params.DownTrack.SetMaxSpatialLayer(
			buffer.VideoQualityToSpatialLayer(livekit.VideoQuality_HIGH, t.params.MediaTrack.ToProto()),
		)
	}
	t.maybeOnBind()
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

func (t *SubscribedTrack) maybeOnBind() {
	if onBind := t.onBind.Load(); onBind != nil && t.bound.Load() {
		go onBind.(func())()
	}
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

	quality := settings.Quality
	if settings.Width > 0 {
		quality = t.MediaTrack().GetQualityForDimension(settings.Width, settings.Height)
	}

	spatial := buffer.VideoQualityToSpatialLayer(quality, t.params.MediaTrack.ToProto())
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
	muted := t.subMuted.Load() || t.pubMuted.Load()
	t.DownTrack().Mute(muted)
}
