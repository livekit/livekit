package buffer

type LayerStats struct {
	TotalPackets uint32
	TotalBytes   uint64
	TotalFrames  uint32
}

// RAJA-REMOVE-START
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
	LostRate               float32
}

// RAJA-REMOVE-END

type StreamStatsWithLayers struct {
	// RAJA-REMOVE-START
	StreamStats StreamStats
	// RAJA-REMOVE-END
	Layers map[int]LayerStats
}
