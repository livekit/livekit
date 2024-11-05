package sendsidebwe

import (
	"go.uber.org/zap/zapcore"
)

type packetInfo struct {
	sendTime  int64
	sendDelta int64
	// SSBWE-TODO: may need a feedback report time to detect stale reports
	// SSBWE-TODO: trim down fields to ensure smallish size, too many 8-byte fields here
	recvTime     int64
	recvDelta    int64
	deltaOfDelta int64
	sn           uint16
	payloadSize  uint16
	headerSize   uint8
	isDeltaValid bool
	isRTX        bool
	// SSBWE-TODO: possibly add the following fields - pertaining to this packet,
	// idea is to be able to traverse back and find last packet with clean signal(s)
	// in order to figure out bitrate at which congestion triggered.
	//    MPETau - Mean Percentage Error trend - potentially an early signal of impending congestion
	//    PeakDetectorSignal - A sustained peak could indicate definite congestion
	//    AcknowledgedBitrate - Bitrate acknowledged by feedback
	//    ProbePacketInfo - When probing, these packets could be analyzed separately to check if probing is successful
}

func (pi *packetInfo) Reset(sn uint16) {
	pi.sn = sn
	pi.sendTime = 0
	pi.sendDelta = 0
	pi.recvTime = 0
	pi.recvDelta = 0
	pi.deltaOfDelta = 0
	pi.isDeltaValid = false
	pi.headerSize = 0
	pi.payloadSize = 0
	pi.isRTX = false
}

func (pi *packetInfo) ResetReceiveAndDeltas() {
	pi.sendDelta = 0
	pi.recvTime = 0
	pi.recvDelta = 0
	pi.deltaOfDelta = 0
	pi.isDeltaValid = false
}

func (pi *packetInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if pi == nil {
		return nil
	}

	e.AddUint16("sn", pi.sn)
	e.AddInt64("sendTime", pi.sendTime)
	e.AddInt64("sendDelta", pi.sendDelta)
	e.AddInt64("recvTime", pi.recvTime)
	e.AddInt64("recvDelta", pi.recvDelta)
	e.AddInt64("deltaOfDelta", pi.deltaOfDelta)
	e.AddUint8("headerSize", pi.headerSize)
	e.AddUint16("payloadSize", pi.payloadSize)
	e.AddBool("isRTX", pi.isRTX)
	return nil
}
