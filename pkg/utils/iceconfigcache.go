package utils

import (
	"time"

	"github.com/jellydator/ttlcache/v3"

	"github.com/livekit/protocol/livekit"
)

const (
	iceConfigTTLMin = 5 * time.Minute
)

type IceConfigCache[T comparable] struct {
	c *ttlcache.Cache[T, *livekit.ICEConfig]
}

func NewIceConfigCache[T comparable](ttl time.Duration) *IceConfigCache[T] {
	cache := ttlcache.New(
		ttlcache.WithTTL[T, *livekit.ICEConfig](max(ttl, iceConfigTTLMin)),
		ttlcache.WithDisableTouchOnHit[T, *livekit.ICEConfig](),
	)
	go cache.Start()

	return &IceConfigCache[T]{cache}
}

func (icc *IceConfigCache[T]) Stop() {
	icc.c.Stop()
}

func (icc *IceConfigCache[T]) Put(key T, iceConfig *livekit.ICEConfig) {
	icc.c.Set(key, iceConfig, ttlcache.DefaultTTL)
}

func (icc *IceConfigCache[T]) Get(key T) *livekit.ICEConfig {
	if it := icc.c.Get(key); it != nil {
		return it.Value()
	}
	return &livekit.ICEConfig{}
}
