package sendsidebwe

import (
	"go.uber.org/zap/zapcore"
)

// SSBWE-TODO: evaluate this struct and clean up as many fields as possible
type packetInfo struct {
	sendTime  int64
	sendDelta int64
	// SSBWE-TODO: may need a feedback report time to detect stale reports
	recvTime       int64
	recvDelta      int64
	sequenceNumber uint16
	size           uint16
	isRTX          bool
	// SSBWE-TODO: possibly add the following fields - pertaining to this packet,
	// idea is to be able to figure out probe start/end and check for bitrate in that window
}

func (pi *packetInfo) Reset(sequenceNumber uint16) {
	pi.sequenceNumber = sequenceNumber
	pi.sendTime = 0
	pi.sendDelta = 0
	pi.recvTime = 0
	pi.recvDelta = 0
	pi.size = 0
	pi.isRTX = false
}

func (pi *packetInfo) ResetReceiveAndDeltas() {
	pi.sendDelta = 0
	pi.recvTime = 0
	pi.recvDelta = 0
}

func (pi *packetInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if pi == nil {
		return nil
	}

	e.AddUint16("sequenceNumber", pi.sequenceNumber)
	e.AddInt64("sendTime", pi.sendTime)
	e.AddInt64("sendDelta", pi.sendDelta)
	e.AddInt64("recvTime", pi.recvTime)
	e.AddInt64("recvDelta", pi.recvDelta)
	e.AddUint16("size", pi.size)
	e.AddBool("isRTX", pi.isRTX)
	return nil
}
