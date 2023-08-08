package sendsidebwe

type packetInfo struct {
	sendTime  int64
	sendDelta int64
	// SSBWE-TODO: may need a feedback report time to detect stale reports
	receiveTime  int64
	receiveDelta int64
	deltaOfDelta int64
	isDeltaValid bool
	headerSize   uint16
	payloadSize  uint16
	isRTX        bool
	// SSBWE-TODO: possibly add the following fields - pertaining to this packet,
	// idea is to be able to traverse back and find last packet with clean signal(s)
	// in order to figure out bitrate at which congestion triggered.
	//    MPETau - Mean Percentage Error trend - potentially an early signal of impending congestion
	//    PeakDetectorSignal - A sustained peak could indicate definite congestion
	//    AcknowledgedBitrate - Bitrate acknowledged by feedback
	//    ProbePacketInfo - When probing, these packets could be analyzed separately to check if probing is successful
}

func (pi *packetInfo) Reset() {
	pi.sendTime = 0
	pi.sendDelta = 0
	pi.receiveTime = 0
	pi.receiveDelta = 0
	pi.deltaOfDelta = 0
	pi.isDeltaValid = false
	pi.headerSize = 0
	pi.payloadSize = 0
	pi.isRTX = false
}

func (pi *packetInfo) ResetReceiveAndDeltas() {
	pi.sendDelta = 0
	pi.receiveTime = 0
	pi.receiveDelta = 0
	pi.deltaOfDelta = 0
	pi.isDeltaValid = false
}
