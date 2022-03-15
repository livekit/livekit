package buffer

import "github.com/livekit/protocol/livekit"

type LayerStats struct {
	TotalPackets uint32
	TotalBytes   uint64
	TotalFrames  uint32
}

type StreamStatsWithLayers struct {
	RTPStats *livekit.RTPStats
	Layers   map[int]LayerStats
}

type ConnectionQualityParams struct {
	LossPercentage float32
	Jitter         float32
	Rtt            uint32
}
