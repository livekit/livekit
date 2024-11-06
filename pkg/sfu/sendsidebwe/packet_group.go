package sendsidebwe

import (
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap/zapcore"
)

// -------------------------------------------------------------

var (
	errOutOfRange = errors.New("packet out of range")
)

// -------------------------------------------------------------

// SSBWE-TODO is this needed?
type queuingRegion int

const (
	queuingRegionJoint queuingRegion = iota
	queuingRegionDisjoint
)

func (q queuingRegion) String() string {
	switch q {
	case queuingRegionJoint:
		return "JOINT"
	case queuingRegionDisjoint:
		return "DISJOINT"
	default:
		return fmt.Sprintf("%d", int(q))
	}
}

// -------------------------------------------------------------

type PacketGroupParams struct {
	Spread time.Duration
}

type PacketGroup struct {
	params PacketGroupParams

	minInitialized bool
	minSendTime int64
	maxSendTime int64
	minRecvTime int64	// SSBWE-TODO: REMOVE
	maxRecvTime int64	// SSBWE-TODO: REMOVE

	numPackets      int
	numBytesPrimary int
	numBytesRTX     int

	aggregateSendDelta int64
	aggregateRecvDelta int64
	groupDelay         int64
	// SSBWE-TODO: check number of zero crossings of the delta jitter
	// if the number of zero crossings is a high ratio of number of packets, it is not stable
}

func NewPacketGroup(params PacketGroupParams) *PacketGroup {
	return &PacketGroup{
		params: params,
	}
}

// SSBWE-TODO - can get a really old packet (out-of-order delivery to the remote)  that could spread things a bunch, how to deal with it?
func (p *PacketGroup) Add(pi *packetInfo) error {
	if p.minInitialized && (pi.sendTime-p.minSendTime) > p.params.Spread.Microseconds() {
		return errOutOfRange
	}

	// sendDelta is delta from this packet's sendTime to the previous acked packet's sendTime,
	// so mark start of window as the sendTime of previous packet.
	/* SSBWE-TODO: is there need to update min after setting initially?
	prevSendTime := pi.sendTime - pi.sendDelta
	if p.minSendTime == 0 || prevSendTime < p.minSendTime {
		p.minSendTime = prevSendTime
	}
	*/
	if !p.minInitialized {
		p.minInitialized = true
		p.minSendTime = pi.sendTime - pi.sendDelta
		p.minRecvTime = pi.recvTime - pi.recvDelta
	}
	if p.maxSendTime == 0 || pi.sendTime > p.maxSendTime {
		p.maxSendTime = pi.sendTime
	}
	if p.maxRecvTime == 0 || pi.recvTime > p.maxRecvTime {
		p.maxRecvTime = pi.recvTime
	}

	p.numPackets++
	if !pi.isRTX {
		p.numBytesPrimary += int(pi.headerSize) + int(pi.payloadSize)
	} else {
		p.numBytesRTX += int(pi.headerSize) + int(pi.payloadSize)
	}

	p.aggregateSendDelta += pi.sendDelta
	p.aggregateRecvDelta += pi.recvDelta
	p.groupDelay += pi.deltaOfDelta
	return nil
}

func (p *PacketGroup) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if p == nil {
		return nil
	}

	e.AddInt64("minSendTime", p.minSendTime)
	e.AddInt64("maxSendTime", p.maxSendTime)
	sendDuration := time.Duration((p.maxSendTime - p.minSendTime) * 1000)
	e.AddDuration("sendDuration", sendDuration)

	e.AddInt64("minRecvTime", p.minRecvTime)
	e.AddInt64("maxRecvTime", p.maxRecvTime)
	recvDuration := time.Duration((p.maxRecvTime - p.minRecvTime) * 1000)
	e.AddDuration("recvDuration", recvDuration)

	e.AddInt("numPackets", p.numPackets)
	if sendDuration != 0 {
		e.AddFloat64("packetRate", float64(p.numPackets)/sendDuration.Seconds())
	}
	e.AddInt("numBytesPrimary", p.numBytesPrimary)
	e.AddInt("numBytesRTX", p.numBytesRTX)

	sendBitRate := float64(0)
	if sendDuration != 0 {
		sendBitRate = float64(p.numBytesPrimary+p.numBytesRTX*8) / sendDuration.Seconds()
		e.AddFloat64("sendBitRate", sendBitRate)
	}

	recvBitRate := float64(0)
	if recvDuration != 0 {
		recvBitRate = float64(p.numBytesPrimary+p.numBytesRTX*8) / recvDuration.Seconds()
		e.AddFloat64("recvBitRate", recvBitRate)
	}

	e.AddInt64("aggregateSendDelta", p.aggregateSendDelta)
	e.AddInt64("aggregateRecvDelta", p.aggregateRecvDelta)
	e.AddInt64("groupDelay", p.groupDelay)
	capturedTrafficRatio := float64(p.aggregateSendDelta) / float64(p.aggregateRecvDelta)
	if capturedTrafficRatio != 0 && sendBitRate != 0 {
		e.AddFloat64("capturedTrafficRatio", capturedTrafficRatio)
		e.AddFloat64("availableBandwidth", min(1.0, capturedTrafficRatio)*sendBitRate)
	}
	return nil
}
