package rtc

import (
	"sync"
	"time"

	"github.com/frostbyte73/go-throttle"

	"github.com/livekit/livekit-server/pkg/config"
)

type rtcpThrottle struct {
	config    config.RTCPThrottleConfig
	mu        sync.RWMutex
	throttles map[uint32]func(func())
}

// github.com/pion/ion-sfu/pkg/sfu/simulcast.go
const (
	fullResolution    = "f"
	halfResolution    = "h"
	quarterResolution = "q"
)

func newRtcpThrottle(conf config.RTCPThrottleConfig) *rtcpThrottle {
	return &rtcpThrottle{
		config:    conf,
		throttles: make(map[uint32]func(func())),
	}
}

func (t *rtcpThrottle) addTrack(ssrc uint32, rid string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var duration time.Duration
	switch rid {
	case fullResolution:
		duration = t.config.HighQuality
	case halfResolution:
		duration = t.config.MidQuality
	case quarterResolution:
		duration = t.config.LowQuality
	default:
		duration = t.config.MidQuality
	}

	t.throttles[ssrc] = throttle.New(duration)
}

func (t *rtcpThrottle) add(ssrc uint32, f func()) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if trackThrottle, ok := t.throttles[ssrc]; ok {
		trackThrottle(f)
	}
}

func (t *rtcpThrottle) removeTrack(ssrc uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if trackThrottle, ok := t.throttles[ssrc]; ok {
		trackThrottle(func() {})
		delete(t.throttles, ssrc)
	}
}

func (t *rtcpThrottle) close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for ssrc, trackThrottle := range t.throttles {
		trackThrottle(func() {})
		delete(t.throttles, ssrc)
	}
}
