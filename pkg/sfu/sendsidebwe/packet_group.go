package sendsidebwe

import (
	"errors"
	"fmt"
	"time"

	"github.com/livekit/protocol/logger"
	"go.uber.org/zap/zapcore"
)

// -------------------------------------------------------------

var (
	errGroupFinalized = errors.New("packet group is finalized")
)

// -------------------------------------------------------------

// SSBWE-TODO is this needed?
type queuingRegion int

const (
	queuingRegionJoint queuingRegion = iota
	queuingRegionDisjoint
	queuingRegionIndeterminate
)

func (q queuingRegion) String() string {
	switch q {
	case queuingRegionJoint:
		return "JOINT"
	case queuingRegionDisjoint:
		return "DISJOINT"
	case queuingRegionIndeterminate:
		return "INDETERMINATE"
	default:
		return fmt.Sprintf("%d", int(q))
	}
}

// -------------------------------------------------------------

type PacketGroupConfig struct {
	MinPackets        int           `yaml:"min_packets,omitempty"`
	MaxWindowDuration time.Duration `yaml:"max_window_duration,omitempty"`
}

var (
	DefaultPacketGroupConfig = PacketGroupConfig{
		MinPackets:        20,
		MaxWindowDuration: 500 * time.Millisecond,
	}
)

type PacketGroupParams struct {
	Config PacketGroupConfig
	Logger logger.Logger
}

type PacketGroup struct {
	params PacketGroupParams

	minInitialized bool
	minSendTime    int64
	maxSendTime    int64
	minRecvTime    int64 // SSBWE-TODO: REMOVE
	maxRecvTime    int64 // SSBWE-TODO: REMOVE

	numPackets      int
	numBytesPrimary int
	numBytesRTX     int

	aggregateSendDelta int64
	aggregateRecvDelta int64
	queuingDelay       int64

	isFinalized bool
	// SSBWE-TODO: check number of zero crossings of the delta jitter
	// if the number of zero crossings is a high ratio of number of packets, it is not stable
}

func NewPacketGroup(params PacketGroupParams, queuingDelay int64) *PacketGroup {
	return &PacketGroup{
		params:       params,
		queuingDelay: queuingDelay,
	}
}

// SSBWE-TODO - can get a really old packet (out-of-order delivery to the remote)  that could spread things a bunch, how to deal with it?
func (p *PacketGroup) Add(pi *packetInfo, piPrev *packetInfo) error {
	if p.isFinalized {
		return errGroupFinalized
	}

	// sendDelta is delta from this packet's sendTime to the previous acked packet's sendTime,
	// so mark start of window as the sendTime of previous packet.
	/* SSBWE-TODO: is there need to update min after setting initially?
	prevSendTime := pi.sendTime - pi.sendDelta
	if p.minSendTime == 0 || prevSendTime < p.minSendTime {
		p.minSendTime = prevSendTime
	}
	*/
	// start window including the gap from this packet to previous packet
	// which would have been in previous group to ensure continuity.
	// although, pi.sendTime - pi.sendDelta should be equal to piPrev.sendTime,
	// it is written as an equation below to emphasize that inter-group gap is
	// covered to maintain continuity.
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

	// in the gap from this packet to prev packet, the previous packet
	// is the one that was transimitted, so count size of previous packet here.
	p.numPackets++
	if !piPrev.isRTX {
		p.numBytesPrimary += int(piPrev.headerSize) + int(piPrev.payloadSize)
	} else {
		p.numBytesRTX += int(piPrev.headerSize) + int(piPrev.payloadSize)
	}

	p.aggregateSendDelta += pi.sendDelta
	p.aggregateRecvDelta += pi.recvDelta

	if p.numPackets == p.params.Config.MinPackets || (pi.sendTime-p.minSendTime) > p.params.Config.MaxWindowDuration.Microseconds() {
		p.isFinalized = true
	}
	return nil
}

func (p *PacketGroup) MinSendTime() (int64, bool) {
	return p.minSendTime, p.minInitialized
}

func (p *PacketGroup) PropagatedQueuingDelay() int64 {
	if !p.isFinalized {
		return 0
	}

	if p.queuingDelay+p.aggregateRecvDelta-p.aggregateSendDelta > 0 {
		return p.queuingDelay + p.aggregateRecvDelta - p.aggregateSendDelta
	}

	return max(0, p.aggregateRecvDelta-p.aggregateSendDelta)
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
	e.AddInt64("queuingDelay", p.queuingDelay)
	e.AddInt64("groupDelay", p.aggregateRecvDelta-p.aggregateSendDelta)
	// SSBWE-TODO: need to do this over a longer range
	capturedTrafficRatio := float64(p.aggregateSendDelta) / float64(p.aggregateRecvDelta)
	if capturedTrafficRatio != 0 && sendBitRate != 0 {
		e.AddFloat64("capturedTrafficRatio", capturedTrafficRatio)
		e.AddFloat64("availableBandwidth", min(1.0, capturedTrafficRatio)*sendBitRate)
	}

	e.AddBool("isFinalized", p.isFinalized)
	return nil
}
