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

type IceConfigCache struct {
	lock    sync.Mutex
	entries map[interface{}]*iceConfigCacheEntry

	stopped atomic.Bool
}

func NewIceConfigCache() *IceConfigCache {
	icc := &IceConfigCache{
		entries: make(map[interface{}]*iceConfigCacheEntry),
	}

	go icc.pruneWorker()
	return icc
}

func (icc *IceConfigCache) Stop() {
	icc.stopped.Store(true)
}

func (icc *IceConfigCache) Put(key interface{}, iceConfig *livekit.ICEConfig, at time.Time) {
	icc.lock.Lock()
	defer icc.lock.Unlock()

	icc.entries[key] = &iceConfigCacheEntry{
		iceConfig:  iceConfig,
		modifiedAt: at,
	}
}

func (icc *IceConfigCache) Get(key interface{}) *livekit.ICEConfig {
	icc.lock.Lock()
	defer icc.lock.Unlock()

	entry, ok := icc.entries[key]
	if !ok || time.Since(entry.modifiedAt) > iceConfigTTL {
		delete(icc.entries, key)
		return &livekit.ICEConfig{}
	}

	return entry.iceConfig
}

func (icc *IceConfigCache) pruneWorker() {
	ticker := time.NewTicker(iceConfigTTL / 2)
	defer ticker.Stop()

	for !icc.stopped.Load() {
		<-ticker.C

		icc.lock.Lock()
		for key, entry := range icc.entries {
			if time.Since(entry.modifiedAt) > iceConfigTTL {
				delete(icc.entries, key)
			}
		}
		icc.lock.Unlock()
	}
}
