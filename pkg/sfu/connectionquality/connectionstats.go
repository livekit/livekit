package connectionquality

import (
	"github.com/livekit/protocol/livekit"
)

type ConnectionStat struct {
	PacketsLost  uint32
	Delay        uint32
	Jitter       uint32
	TotalPackets uint32
	LastSeqNum   uint32
}

type ConnectionStats struct {
	Curr  *ConnectionStat
	Prev  *ConnectionStat
	Score float64
}

func NewConnectionStats() *ConnectionStats {
	return &ConnectionStats{Curr: &ConnectionStat{}, Prev: &ConnectionStat{}}
}

func getTotalPackets(curSN, prevSN uint32) uint32 {
	delta := curSN - prevSN
	// lower 16 bits is the counter
	counter := uint16(delta)
	// upper 16 bits contains the cycles/wrap
	cycles := uint16(delta >> 16)
	increment := (uint32(cycles) * (1 << 16)) + uint32(counter)

	return increment
}

func (cs *ConnectionStats) CalculateScore(kind livekit.TrackType) {
	// update feedback stats
	current := cs.Curr
	previous := cs.Prev
	// Update TotalPackets from SeqNum here
	current.TotalPackets += getTotalPackets(current.LastSeqNum, previous.LastSeqNum)

	cs.Score = ConnectionScore(current, previous, kind)

	// store previous stats
	cs.Prev = current
	cs.Curr = &ConnectionStat{TotalPackets: previous.TotalPackets}

	return
}
