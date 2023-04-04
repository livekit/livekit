package buffer

type StreamStatsWithLayers struct {
	RTPStats *RTPDeltaInfo
	Layers   map[int32]*RTPDeltaInfo
}
