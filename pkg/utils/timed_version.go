package utils

import (
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

type TimedVersionGenerator interface {
	New() *TimedVersion
}

func NewDefaultTimedVersionGenerator() TimedVersionGenerator {
	return &timedVersionGenerator{}
}

type timedVersionGenerator struct {
	lock  sync.Mutex
	ts    int64
	ticks int32
}

func (g *timedVersionGenerator) New() *TimedVersion {
	ts := time.Now().UnixMicro()
	var ticks int32

	g.lock.Lock()
	if ts <= g.ts {
		g.ticks++
		ts = g.ts
		ticks = g.ticks
	} else {
		g.ts = ts
		g.ticks = 0
	}
	g.lock.Unlock()

	return &TimedVersion{
		ts:    ts,
		ticks: ticks,
	}
}

type TimedVersion struct {
	lock  sync.RWMutex
	ts    int64
	ticks int32
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

func (t *TimedVersion) String() string {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return fmt.Sprintf("%d.%d", t.ts, t.ticks)
}
