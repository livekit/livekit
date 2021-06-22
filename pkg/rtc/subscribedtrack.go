package rtc

import (
	"time"

	"github.com/bep/debounce"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/webrtc/v3"

	livekit "github.com/livekit/livekit-server/proto"
	"github.com/livekit/protocol/utils"
)

const (
	subscriptionDebounceInterval = 10 * time.Millisecond
)

type SubscribedTrack struct {
	dt        *sfu.DownTrack
	subMuted  utils.AtomicFlag
	pubMuted  utils.AtomicFlag
	debouncer func(func())
}

func NewSubscribedTrack(dt *sfu.DownTrack) *SubscribedTrack {
	return &SubscribedTrack{
		dt:        dt,
		debouncer: debounce.New(subscriptionDebounceInterval),
	}
}

func (t *SubscribedTrack) ID() string {
	return t.dt.ID()
}

func (t *SubscribedTrack) DownTrack() *sfu.DownTrack {
	return t.dt
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
			_ = t.dt.SwitchSpatialLayer(spatialLayerForQuality(quality), true)
		}
	})
}

func (t *SubscribedTrack) updateDownTrackMute() {
	muted := t.subMuted.Get() || t.pubMuted.Get()
	t.dt.Mute(muted)
}

func spatialLayerForQuality(quality livekit.VideoQuality) int64 {
	switch quality {
	case livekit.VideoQuality_LOW:
		return 0
	case livekit.VideoQuality_MEDIUM:
		return 1
	default:
		return 2
	}
}
