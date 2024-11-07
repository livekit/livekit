package sendsidebwe

import (
	"go.uber.org/zap/zapcore"
)

// SSBWE-TODO: evaluate this struct and clean up as many fields as possible
type packetInfo struct {
	sendTime  int64
	sendDelta int64
	// SSBWE-TODO: may need a feedback report time to detect stale reports
	recvTime    int64
	recvDelta   int64
	sn          uint16
	payloadSize uint16
	headerSize  uint8
	isRTX       bool
	// SSBWE-TODO: possibly add the following fields - pertaining to this packet,
	// idea is to be able to figure out probe start/end and check for bitrate in that window
}

func (pi *packetInfo) Reset(sn uint16) {
	pi.sn = sn
	pi.sendTime = 0
	pi.sendDelta = 0
	pi.recvTime = 0
	pi.recvDelta = 0
	pi.headerSize = 0
	pi.payloadSize = 0
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

	e.AddUint16("sn", pi.sn)
	e.AddInt64("sendTime", pi.sendTime)
	e.AddInt64("sendDelta", pi.sendDelta)
	e.AddInt64("recvTime", pi.recvTime)
	e.AddInt64("recvDelta", pi.recvDelta)
	e.AddUint16("payloadSize", pi.payloadSize)
	e.AddBool("isRTX", pi.isRTX)
	return nil
}
