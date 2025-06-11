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

// Add adds a message to the cache if there is a gap between the last sequence number and cached messages then return the cache State:
//   - MigrationDataCacheStateWaiting: waiting for the next packet (lastSeq + 1) of last sequence from old node
//   - MigrationDataCacheStateTimeout: the next packet is not received before the expiredAt, participant will
//     continue to process the reliable messages, subscribers will see the gap after the publisher migration
//   - MigrationDataCacheStateDone: the next packet is received, participant can continue to process the reliable messages
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
