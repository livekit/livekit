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

// -------------------------------------------------------------

type stat struct {
	numPackets int
	numBytes   int
}

func (s *stat) add(size int) {
	s.numPackets++
	s.numBytes += size
}

func (s *stat) getNumPackets() int {
	return s.numPackets
}

func (s *stat) getNumBytes() int {
	return s.numBytes
}

func (s stat) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddInt("numPackets", s.numPackets)
	e.AddInt("numBytes", s.numBytes)
	return nil
}

// -------------------------------------------------------------

type classStat struct {
	primary stat
	rtx     stat
}

func (c *classStat) add(size int, isRTX bool) {
	if isRTX {
		c.rtx.add(size)
	} else {
		c.primary.add(size)
	}
}

func (c *classStat) numPackets() int {
	return c.primary.getNumPackets() + c.rtx.getNumPackets()
}

func (c *classStat) numBytes() int {
	return c.primary.getNumBytes() + c.rtx.getNumBytes()
}

func (c classStat) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddObject("primary", c.primary)
	e.AddObject("rtx", c.rtx)
	return nil
}

// -------------------------------------------------------------

type PacketGroupParams struct {
	Config PacketGroupConfig
	Logger logger.Logger
}

type PacketGroup struct {
	params PacketGroupParams

	minSendInitialized bool
	minSendTime    int64
	maxSendTime    int64

	minRecvInitialized bool // for information only
	minRecvTime    int64 // for information only
	maxRecvTime    int64 // for information only

	acked classStat

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

func (p *PacketGroup) Add(pi *packetInfo, piPrev *packetInfo) error {
	if p.isFinalized {
		return errGroupFinalized
	}

	// start window including the gap from this packet to previous packet
	// which would have been in previous group to ensure continuity.
	// although, pi.sendTime - pi.sendDelta should be equal to piPrev.sendTime,
	// it is written as an equation below to emphasize that inter-group gap is
	// covered to maintain continuity.
	// SSBWE-TODO: check if older packets can get in here, i. e. packets before min
	if !p.minSendInitialized {
		p.minSendInitialized = true
		p.minSendTime = pi.sendTime - pi.sendDelta
	}
	if p.maxSendTime == 0 || pi.sendTime > p.maxSendTime {
		p.maxSendTime = pi.sendTime
	}

	if !p.minRecvInitialized {
		p.minRecvInitialized = true
		p.minRecvTime = pi.recvTime - pi.recvDelta
	}
	if p.maxRecvTime == 0 || pi.recvTime > p.maxRecvTime {
		p.maxRecvTime = pi.recvTime
	}

	// in the gap from this packet to prev packet, the packet that was transmitted
	// is the previous packet, so count size of previous packet here for this delta.
	p.acked.add(int(piPrev.headerSize)+int(piPrev.payloadSize), piPrev.isRTX)

	p.aggregateSendDelta += pi.sendDelta
	p.aggregateRecvDelta += pi.recvDelta

	if p.acked.numPackets() == p.params.Config.MinPackets || (pi.sendTime-p.minSendTime) > p.params.Config.MaxWindowDuration.Microseconds() {
		p.isFinalized = true
	}
	return nil
}

func (p *PacketGroup) MinSendTime() (int64, bool) {
	return p.minSendTime, p.minSendInitialized
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

	// SSBWE-TODO: should traffic include lost bytes as well to calculate rate? CTR does not capture loss
	// SSBWE-TODO: not including lost means send side rate could be under calculated if there are a bunch of losses
	return p.minSendTime, p.maxSendTime - p.minSendTime, p.acked.numBytes(), min(1.0, capturedTrafficRatio)
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

	e.AddObject("acked", p.acked)

	sendBitRate := float64(0)
	if sendDuration != 0 {
		sendBitRate = float64(p.acked.numBytes()*8) / sendDuration.Seconds()
		e.AddFloat64("sendBitRate", sendBitRate)
	}

	recvBitRate := float64(0)
	if recvDuration != 0 {
		recvBitRate = float64(p.acked.numBytes()*8) / recvDuration.Seconds()
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
			e.AddFloat64("estimatedAvailableChannelCapacity", min(1.0, capturedTrafficRatio)*sendBitRate)
		}
	}

	e.AddBool("isFinalized", p.isFinalized)
	return nil
}
