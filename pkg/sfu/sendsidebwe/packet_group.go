package sendsidebwe

import (
	"errors"
	"time"

	"github.com/livekit/protocol/logger"
	"go.uber.org/zap/zapcore"
)

// -------------------------------------------------------------

var (
	errGroupFinalized = errors.New("packet group is finalized")
)

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

	numPacketsPrimary      int
	numBytesPrimary int

	numPacketsRTX      int
	numBytesRTX     int

	aggregateSendDelta int64
	aggregateRecvDelta int64
	queuingDelay       int64

	isFinalized bool
}

func NewPacketGroup(params PacketGroupParams, queuingDelay int64) *PacketGroup {
	return &PacketGroup{
		params:       params,
		queuingDelay: queuingDelay,
	}
}

// SSBWE-TODO - can get a really old packet (out-of-order delivery to the remote)  that could spread things a bunch, how to deal with it?
// SSBWE-TODO - should record lost packets also as that should be included in send side bitrate
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
	if !piPrev.isRTX {
		p.numPacketsPrimary++
		p.numBytesPrimary += int(piPrev.headerSize) + int(piPrev.payloadSize)
	} else {
		p.numPacketsRTX++
		p.numBytesRTX += int(piPrev.headerSize) + int(piPrev.payloadSize)
	}

	p.aggregateSendDelta += pi.sendDelta
	p.aggregateRecvDelta += pi.recvDelta

	if (p.numPacketsPrimary + p.numPacketsRTX) == p.params.Config.MinPackets || (pi.sendTime-p.minSendTime) > p.params.Config.MaxWindowDuration.Microseconds() {
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

func (p *PacketGroup) SendDuration() int64 {
	if !p.isFinalized {
		return 0
	}

	return p.maxSendTime - p.minSendTime
}

func (p *PacketGroup) Traffic() (int64, int64, int, float64) {
	capturedTrafficRatio := float64(0.0)
	if p.aggregateRecvDelta != 0 {
		capturedTrafficRatio = float64(p.aggregateSendDelta) / float64(p.aggregateRecvDelta)
	}

	return p.minSendTime, p.maxSendTime - p.minSendTime, p.numBytesPrimary + p.numBytesRTX, min(1.0, capturedTrafficRatio)
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

	e.AddInt("numPacketsPrimary", p.numPacketsPrimary)
	if sendDuration != 0 {
		e.AddFloat64("packetRatePrimary", float64(p.numPacketsPrimary)/sendDuration.Seconds())
	}
	e.AddInt("numBytesPrimary", p.numBytesPrimary)

	e.AddInt("numPacketsRTX", p.numPacketsRTX)
	if sendDuration != 0 {
		e.AddFloat64("packetRateRTX", float64(p.numPacketsRTX)/sendDuration.Seconds())
	}
	e.AddInt("numBytesRTX", p.numBytesRTX)

	sendBitRate := float64(0)
	if sendDuration != 0 {
		sendBitRate = float64((p.numBytesPrimary+p.numBytesRTX)*8) / sendDuration.Seconds()
		e.AddFloat64("sendBitRate", sendBitRate)
	}

	recvBitRate := float64(0)
	if recvDuration != 0 {
		recvBitRate = float64((p.numBytesPrimary+p.numBytesRTX)*8) / recvDuration.Seconds()
		e.AddFloat64("recvBitRate", recvBitRate)
	}

	e.AddInt64("aggregateSendDelta", p.aggregateSendDelta)
	e.AddInt64("aggregateRecvDelta", p.aggregateRecvDelta)
	e.AddInt64("queuingDelay", p.queuingDelay)
	e.AddInt64("groupDelay", p.aggregateRecvDelta-p.aggregateSendDelta)
	if p.aggregateRecvDelta != 0 {
		capturedTrafficRatio := float64(p.aggregateSendDelta) / float64(p.aggregateRecvDelta)
		if capturedTrafficRatio != 0 && sendBitRate != 0 {
			e.AddFloat64("capturedTrafficRatio", capturedTrafficRatio)
			e.AddFloat64("receiveBitrate", min(1.0, capturedTrafficRatio)*sendBitRate)
		}
	}

	e.AddBool("isFinalized", p.isFinalized)
	return nil
}
