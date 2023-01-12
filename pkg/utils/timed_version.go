package utils

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

type TimedVersion struct {
	lock  sync.RWMutex
	ts    int64
	ticks int32
}

func NewTimedVersion(at time.Time, ticks int32) *TimedVersion {
	return &TimedVersion{
		ts:    at.UnixMicro(),
		ticks: ticks,
	}
}

func NewTimedVersionFromProto(ptv *livekit.TimedVersion) *TimedVersion {
	return &TimedVersion{
		ts:    ptv.UnixMicro,
		ticks: ptv.Ticks,
	}
}

func (t *TimedVersion) Update(other *TimedVersion) {
	t.lock.Lock()
	if other.After(t) {
		t.ts = other.ts
		t.ticks = other.ticks
	} else {
		t.ticks++
	}
	t.lock.Unlock()
}

func (t *TimedVersion) After(other *TimedVersion) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if t.ts == other.ts {
		return t.ticks > other.ticks
	}

	return t.ts > other.ts
}

func (t *TimedVersion) ToProto() *livekit.TimedVersion {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return &livekit.TimedVersion{
		UnixMicro: t.ts,
		Ticks:     t.ticks,
	}
}
