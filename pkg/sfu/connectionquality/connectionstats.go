package connectionquality

import (
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
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
	Kind  string
}

func NewConnectionStats(kind string) *ConnectionStats {
	return &ConnectionStats{Kind: kind, Curr: &ConnectionStat{}, Prev: &ConnectionStat{}, Score: 5.0}
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

func (cs *ConnectionStats) CalculateAudioScore() float64 {
	// update feedback stats
	current := cs.Curr
	previous := cs.Prev

	// Update TotalPackets from SeqNum here
	current.TotalPackets += getTotalPackets(current.LastSeqNum, previous.LastSeqNum)
	cs.Score = ConnectionScore(current, previous, livekit.TrackType_AUDIO)

	if cs.Score < 4 {
		cs.DebugInfo()
	}
	// store previous stats
	cs.Prev = current
	cs.Curr = &ConnectionStat{TotalPackets: cs.Prev.TotalPackets, PacketsLost: cs.Prev.PacketsLost}

	return cs.Score
}

func (cs *ConnectionStats) DebugInfo() {
	logger.Debugw("audio connection stats", "kind", cs.Kind, "score", cs.Score, "delay", cs.Curr.Delay,
		"jitter", cs.Curr.Jitter, "delta_packets", cs.Curr.TotalPackets-cs.Prev.TotalPackets,
		"delta_packets_lost", cs.Curr.PacketsLost-cs.Prev.PacketsLost)
}
