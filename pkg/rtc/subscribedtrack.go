package rtc

import (
	"time"

	"github.com/bep/debounce"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/webrtc/v3"
)

const (
	subscriptionDebounceInterval = 100 * time.Millisecond
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
			err := t.dt.SwitchSpatialLayer(spatialLayerForQuality(quality), true)
			if err == sfu.ErrSpatialLayerNotFound && quality != livekit.VideoQuality_MEDIUM {
				// try to switch to middle layer
				_ = t.dt.SwitchSpatialLayer(spatialLayerForQuality(livekit.VideoQuality_MEDIUM), true)
			}
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
