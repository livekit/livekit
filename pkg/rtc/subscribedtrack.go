package rtc

import (
	"time"

	"github.com/bep/debounce"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
)

const (
	subscriptionDebounceInterval = 100 * time.Millisecond
)

type SubscribedTrackParams struct {
	PublisherID        livekit.ParticipantID
	PublisherIdentity  livekit.ParticipantIdentity
	SubscriberID       livekit.ParticipantID
	SubscriberIdentity livekit.ParticipantIdentity
	MediaTrack         types.MediaTrack
	DownTrack          *sfu.DownTrack
	AdaptiveStream     bool
}

type SubscribedTrack struct {
	params   SubscribedTrackParams
	subMuted atomic.Bool
	pubMuted atomic.Bool
	settings atomic.Value // *livekit.UpdateTrackSettings

	onBind func()

	debouncer func(func())
}

func NewSubscribedTrack(params SubscribedTrackParams) *SubscribedTrack {
	s := &SubscribedTrack{
		params:    params,
		debouncer: debounce.New(subscriptionDebounceInterval),
	}

	if !s.params.AdaptiveStream {
		s.params.DownTrack.SetMaxSpatialLayer(SpatialLayerForQuality(livekit.VideoQuality_HIGH))
	}
	return s
}

func (t *SubscribedTrack) OnBind(f func()) {
	t.onBind = f
}

func (t *SubscribedTrack) Bound() {
	if t.onBind != nil {
		t.onBind()
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

func (t *SubscribedTrack) SubscriberID() livekit.ParticipantID {
	return t.params.SubscriberID
}

func (t *SubscribedTrack) SubscriberIdentity() livekit.ParticipantIdentity {
	return t.params.SubscriberIdentity
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

	settings, ok := t.settings.Load().(*livekit.UpdateTrackSettings)
	if !ok {
		return
	}

	quality := settings.Quality
	if settings.Width > 0 {
		quality = t.MediaTrack().GetQualityForDimension(settings.Width, settings.Height)
	}
	t.DownTrack().SetMaxSpatialLayer(SpatialLayerForQuality(quality))
}

func (t *SubscribedTrack) updateDownTrackMute() {
	muted := t.subMuted.Load() || t.pubMuted.Load()
	t.DownTrack().Mute(muted)
}
