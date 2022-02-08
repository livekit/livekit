package buffer

type LayerStats struct {
	TotalPackets uint32
	TotalBytes   uint64
	TotalFrames  uint32
}

type StreamStats struct {
	TotalPrimaryPackets    uint32
	TotalPrimaryBytes      uint64
	TotalRetransmitPackets uint32
	TotalRetransmitBytes   uint64
	TotalPaddingPackets    uint32
	TotalPaddingBytes      uint64
	TotalPacketsLost       uint32
	TotalFrames            uint32
	RTT                    uint32
	Jitter                 float64
	TotalNACKs             uint32
	TotalPLIs              uint32
	TotalFIRs              uint32
}

type StreamStatsWithLayers struct {
	StreamStats StreamStats
	Layers      map[int]LayerStats
}
