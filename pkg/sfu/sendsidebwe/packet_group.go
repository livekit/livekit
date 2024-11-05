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

	minSendTime int64
	maxSendTime int64

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
	if p.minSendTime != 0 && (pi.sendTime-p.minSendTime) > p.params.Spread.Microseconds() {
		return errOutOfRange
	}

	if p.minSendTime == 0 || pi.sendTime < p.minSendTime {
		p.minSendTime = pi.sendTime
	}
	if p.maxSendTime == 0 || pi.sendTime > p.maxSendTime {
		p.maxSendTime = pi.sendTime
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
	duration := time.Duration((p.maxSendTime - p.minSendTime) * 1000)
	e.AddDuration("duration", duration)

	e.AddInt("numPackets", p.numPackets)
	if duration != 0 {
		e.AddFloat64("packetRate", float64(p.numPackets)/duration.Seconds())
	}
	e.AddInt("numBytesPrimary", p.numBytesPrimary)
	e.AddInt("numBytesRTX", p.numBytesRTX)
	bitRate := float64(0)
	if duration != 0 {
		bitRate = float64(p.numBytesPrimary+p.numBytesRTX*8) / duration.Seconds()
		e.AddFloat64("bitRate", bitRate)
	}

	e.AddInt64("aggregateSendDelta", p.aggregateSendDelta)
	e.AddInt64("aggregateRecvDelta", p.aggregateRecvDelta)
	e.AddInt64("groupDelay", p.groupDelay)
	capturedTrafficRatio := float64(p.aggregateSendDelta) / float64(p.aggregateRecvDelta)
	if capturedTrafficRatio != 0 && bitRate != 0 {
		e.AddFloat64("capturedTrafficRatio", capturedTrafficRatio)
		e.AddFloat64("availableBandwidth", max(1.0, capturedTrafficRatio)*bitRate)
	}
	return nil
}
