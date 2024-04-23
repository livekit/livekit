package utils

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"go.uber.org/atomic"
)

const (
	iceConfigTTL = 5 * time.Minute
)

type iceConfigCacheEntry struct {
	iceConfig  *livekit.ICEConfig
	modifiedAt time.Time
}

type IceConfigCache[T comparable] struct {
	lock    sync.Mutex
	ttl     time.Duration
	entries map[T]*iceConfigCacheEntry

	stopped atomic.Bool
}

func NewIceConfigCache[T comparable](ttl time.Duration) *IceConfigCache[T] {
	icc := &IceConfigCache[T]{
		entries: make(map[T]*iceConfigCacheEntry),
	}

	if ttl < iceConfigTTL {
		icc.ttl = iceConfigTTL
	} else {
		icc.ttl = ttl
	}

	go icc.pruneWorker()
	return icc
}

func (icc *IceConfigCache[T]) Stop() {
	icc.stopped.Store(true)
}

func (icc *IceConfigCache[T]) Put(key T, iceConfig *livekit.ICEConfig, at time.Time) {
	icc.lock.Lock()
	defer icc.lock.Unlock()

	icc.entries[key] = &iceConfigCacheEntry{
		iceConfig:  iceConfig,
		modifiedAt: at,
	}
}

func (icc *IceConfigCache[T]) Get(key T) *livekit.ICEConfig {
	icc.lock.Lock()
	defer icc.lock.Unlock()

	entry, ok := icc.entries[key]
	if !ok || time.Since(entry.modifiedAt) > icc.ttl {
		delete(icc.entries, key)
		return &livekit.ICEConfig{}
	}

	return entry.iceConfig
}

func (icc *IceConfigCache[T]) pruneWorker() {
	ticker := time.NewTicker(icc.ttl / 2)
	defer ticker.Stop()

	for !icc.stopped.Load() {
		<-ticker.C

		icc.lock.Lock()
		for key, entry := range icc.entries {
			if time.Since(entry.modifiedAt) > icc.ttl {
				delete(icc.entries, key)
			}
		}
		icc.lock.Unlock()
	}
}
