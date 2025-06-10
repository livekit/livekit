package rtc

import (
	"time"

	"github.com/livekit/protocol/livekit"
)

type MigrationDataCacheState int

const (
	MigrationDataCacheStateWaiting MigrationDataCacheState = iota
	MigrationDataCacheStateTimeout
	MigrationDataCacheStateDone
)

type MigrationDataCache struct {
	lastSeq   uint32
	pkts      []*livekit.DataPacket
	state     MigrationDataCacheState
	expiredAt time.Time
}

func NewMigrationDataCache(lastSeq uint32, expiredAt time.Time) *MigrationDataCache {
	return &MigrationDataCache{
		lastSeq:   lastSeq,
		expiredAt: expiredAt,
	}
}

// Add adds a message to the cache if there is a gap between the last sequence number and cached messages then return false.
func (c *MigrationDataCache) Add(pkt *livekit.DataPacket) MigrationDataCacheState {
	if c.state == MigrationDataCacheStateDone || c.state == MigrationDataCacheStateTimeout {
		return c.state
	}

	if pkt.Sequence <= c.lastSeq {
		return c.state
	}

	if pkt.Sequence == c.lastSeq+1 {
		c.state = MigrationDataCacheStateDone
		return c.state
	}

	c.pkts = append(c.pkts, pkt)
	if time.Now().After(c.expiredAt) {
		c.state = MigrationDataCacheStateTimeout
	}
	return c.state
}

func (c *MigrationDataCache) Get() []*livekit.DataPacket {
	return c.pkts
}
