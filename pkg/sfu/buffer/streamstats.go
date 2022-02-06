package buffer

type StreamStats struct {
	TotalPrimaryPackets    uint32
	TotalPrimaryBytes      uint64
	TotalRetransmitPackets uint32
	TotalRetransmitBytes   uint64
	TotalPaddingPackets    uint32
	TotalPaddingBytes      uint64
	TotalPacketsLost       uint32
	TotalFrames            uint32
	// RAJA-TODO RTT
	Jitter     float64
	TotalNACKs uint32
	TotalPLIs  uint32
	TotalFIRs  uint32
}

func (ss *StreamStats) UpdatePrimary(packetLen int, marker bool) {
	ss.TotalPrimaryPackets++
	ss.TotalPrimaryBytes += uint64(packetLen)
	if marker {
		ss.TotalFrames++
	}
}

func (ss *StreamStats) UpdateRtx(packetLen int) {
	ss.TotalRetransmitPackets++
	ss.TotalRetransmitBytes += uint64(packetLen)
}

func (ss *StreamStats) UpdatePadding(packetLen int) {
	ss.TotalPaddingPackets++
	ss.TotalPaddingBytes += uint64(packetLen)
}
