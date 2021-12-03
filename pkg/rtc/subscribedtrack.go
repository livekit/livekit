package rtc

import (
	"time"

	"github.com/bep/debounce"
	"github.com/livekit/livekit-server/pkg/sfu"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/pion/webrtc/v3"
)

const (
	subscriptionDebounceInterval = 100 * time.Millisecond
)

type SubscribedTrack struct {
	dt                *sfu.DownTrack
	publisherIdentity string
	subMuted          utils.AtomicFlag
	pubMuted          utils.AtomicFlag

	debouncer func(func())
}

func NewSubscribedTrack(publisherIdentity string, dt *sfu.DownTrack) *SubscribedTrack {
	return &SubscribedTrack{
		publisherIdentity: publisherIdentity,
		dt:                dt,
		debouncer:         debounce.New(subscriptionDebounceInterval),
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

func (t *SubscribedTrack) SubscribeLossPercentage() uint32 {
	return FixedPointToPercent(t.DownTrack().CurrentMaxLossFraction())
}

// has subscriber indicated it wants to mute this track
func (t *SubscribedTrack) IsMuted() bool {
	return t.subMuted.Get()
}

func (t *SubscribedTrack) SetPublisherMuted(muted bool) {
	t.pubMuted.TrySet(muted)
	t.updateDownTrackMute()
}

func (t *SubscribedTrack) UpdateSubscriberSettings(enabled bool, quality livekit.VideoQuality) {
	t.debouncer(func() {
		t.subMuted.TrySet(!enabled)
		t.updateDownTrackMute()
		if enabled && t.dt.Kind() == webrtc.RTPCodecTypeVideo {
			t.dt.SetMaxSpatialLayer(spatialLayerForQuality(quality))
		}
	})
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
