package connectionquality

import "sync"

type ConnectionStat struct {
	PacketsLost  uint32
	Delay        uint32
	Jitter       uint32
	TotalPackets uint32
	LastSeqNum   uint32
	TotalBytes   uint64
	NackCount    int32
	PliCount     int32
	FirCount     int32
	Score        float64
}

type ConnectionStats struct {
	Lock sync.Mutex
	ConnectionStat
	Prev *ConnectionStat
}

func NewConnectionStats() *ConnectionStats {
	return &ConnectionStats{Prev: &ConnectionStat{}, ConnectionStat: ConnectionStat{Score: 4.0}}
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

type DeltaStats struct {
	TotalPackets uint32
	PacketsLost  uint32
	TotalBytes   uint64
}

func (cs *ConnectionStats) UpdateStats(totalBytes uint64) DeltaStats {
	// update feedback stats
	previous := cs.Prev

	// Update TotalPackets from SeqNum here
	cs.TotalPackets += getTotalPackets(cs.LastSeqNum, previous.LastSeqNum)
	cs.TotalBytes = totalBytes

	var delta DeltaStats

	delta.TotalPackets = cs.TotalPackets - previous.TotalPackets
	delta.PacketsLost = cs.PacketsLost - previous.PacketsLost
	delta.TotalBytes = cs.TotalBytes - previous.TotalBytes

	// store previous stats
	cs.Prev = &ConnectionStat{TotalPackets: cs.TotalPackets, PacketsLost: cs.PacketsLost, TotalBytes: cs.TotalBytes}

	return delta
}
