package buffer

// RAJA-REMOVE
type LayerStats struct {
	Packets uint32
	Bytes   uint64
	Frames  uint32
}

type StreamStatsWithLayers struct {
	RTPStats *RTPDeltaInfo
	// RAJA-REMOVE Layers   map[int]LayerStats
	Layers map[int32]*RTPDeltaInfo
}

// RAJA-REMOVE
type ConnectionQualityParams struct {
	LossPercentage float32
	Jitter         float32
	Rtt            uint32
}
