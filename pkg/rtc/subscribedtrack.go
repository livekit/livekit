package rtc

import (
	"sync/atomic"
	"time"

	"github.com/bep/debounce"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
)

const (
	subscriptionDebounceInterval = 100 * time.Millisecond
)

type SubscribedTrack struct {
	publishedTrack    types.MediaTrack
	dt                *sfu.DownTrack
	publisherIdentity string
	subMuted          utils.AtomicFlag
	pubMuted          utils.AtomicFlag
	settings          atomic.Value // *livekit.UpdateTrackSettings

	onBind func()

	debouncer func(func())
}

func NewSubscribedTrack(mediaTrack types.MediaTrack, publisherIdentity string, dt *sfu.DownTrack) *SubscribedTrack {
	return &SubscribedTrack{
		publishedTrack:    mediaTrack,
		publisherIdentity: publisherIdentity,
		dt:                dt,
		debouncer:         debounce.New(subscriptionDebounceInterval),
	}
}

func (t *SubscribedTrack) OnBind(f func()) {
	t.onBind = f
}

func (t *SubscribedTrack) Bound() {
	if t.onBind != nil {
		t.onBind()
	}
}

func (t *SubscribedTrack) ID() string {
	return t.dt.ID()
}

func (t *SubscribedTrack) PublisherIdentity() string {
	return t.publisherIdentity
}

func (t *SubscribedTrack) DownTrack() *sfu.DownTrack {
	return t.dt
}

func (t *SubscribedTrack) PublishedTrack() types.MediaTrack {
	return t.publishedTrack
}

// has subscriber indicated it wants to mute this track
func (t *SubscribedTrack) IsMuted() bool {
	return t.subMuted.Get()
}

func (t *SubscribedTrack) SetPublisherMuted(muted bool) {
	t.pubMuted.TrySet(muted)
	t.updateDownTrackMute()
}

func (t *SubscribedTrack) UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings) {
	visibilityChanged := t.subMuted.TrySet(settings.Disabled)
	t.settings.Store(settings)
	// avoid frequent changes to mute & video layers, unless it became visible
	if visibilityChanged && !settings.Disabled {
		t.UpdateVideoLayer()
	} else {
		t.debouncer(t.UpdateVideoLayer)
	}
}

func (t *SubscribedTrack) UpdateVideoLayer() {
	t.updateDownTrackMute()
	if t.subMuted.Get() || t.dt.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}
	settings, ok := t.settings.Load().(*livekit.UpdateTrackSettings)
	if !ok {
		return
	}

	quality := settings.Quality
	if settings.Width > 0 {
		quality = t.publishedTrack.GetQualityForDimension(settings.Width, settings.Height)
	}
	t.dt.SetMaxSpatialLayer(spatialLayerForQuality(quality))
}

func (t *SubscribedTrack) updateDownTrackMute() {
	muted := t.subMuted.Get() || t.pubMuted.Get()
	t.dt.Mute(muted)
}

func spatialLayerForQuality(quality livekit.VideoQuality) int32 {
	switch quality {
	case livekit.VideoQuality_LOW:
		return 0
	case livekit.VideoQuality_MEDIUM:
		return 1
	default:
		return 2
	}
}
