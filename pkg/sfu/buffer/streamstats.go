package buffer

type LayerStats struct {
	Packets uint32
	Bytes   uint64
	Frames  uint32
}

type StreamStatsWithLayers struct {
	RTPStats *RTPDeltaInfo
	Layers   map[int]LayerStats
}

type ConnectionQualityParams struct {
	LossPercentage float32
	Jitter         float32
	Rtt            uint32
}
