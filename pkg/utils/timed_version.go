package utils

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

type TimedVersion struct {
	lock  sync.RWMutex
	at    time.Time
	ticks int32
}

func NewTimedVersion(at time.Time, ticks int32) *TimedVersion {
	return &TimedVersion{
		at:    at,
		ticks: ticks,
	}
}

func NewTimedVersionFromProto(proto *livekit.TimedVersion) *TimedVersion {
	return &TimedVersion{
		at:    time.UnixMicro(proto.UnixMicro),
		ticks: proto.Ticks,
	}
}

func (t *TimedVersion) Update(at time.Time) {
	t.lock.Lock()
	if at.After(t.at) {
		t.at = at
		t.ticks = 0
	} else {
		t.ticks++
	}
	t.lock.Unlock()
}

func (t *TimedVersion) After(other *TimedVersion) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if t.at.Equal(other.at) {
		return t.ticks > other.ticks
	}

	return t.at.After(other.at)
}

func (t *TimedVersion) ToProto() *livekit.TimedVersion {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return &livekit.TimedVersion{
		UnixMicro: t.at.UnixMicro(),
		Ticks:     t.ticks,
	}
}
