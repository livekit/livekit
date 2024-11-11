package sendsidebwe

import (
	"errors"
	"math"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"go.uber.org/zap/zapcore"
)

// -------------------------------------------------------------

var (
	errGroupFinalized = errors.New("packet group is finalized")
	errOldPacket      = errors.New("packet is older than packet group start")
)

// -------------------------------------------------------------

type PacketGroupConfig struct {
	MinPackets        int           `yaml:"min_packets,omitempty"`
	MaxWindowDuration time.Duration `yaml:"max_window_duration,omitempty"`

	// should have at least this fraction of `MinPackets` for loss penalty consideration
	LossPenaltyMinPacketsRatio float64 `yaml:"loss_penalty_min_packet_ratio,omitempty"`
	LossPenaltyFactor          float64 `yaml:"loss_penalty_factor,omitempty"`
}

var (
	DefaultPacketGroupConfig = PacketGroupConfig{
		MinPackets:                 20,
		MaxWindowDuration:          500 * time.Millisecond,
		LossPenaltyMinPacketsRatio: 0.5,
		LossPenaltyFactor:          0.25,
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

func (s *stat) remove(size int) {
	s.numPackets--
	s.numBytes -= size
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

func (c *classStat) remove(size int, isRTX bool) {
	if isRTX {
		c.rtx.remove(size)
	} else {
		c.primary.remove(size)
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

type packetGroupParams struct {
	Config PacketGroupConfig
	Logger logger.Logger
}

type packetGroup struct {
	params packetGroupParams

	minSequenceNumber uint64
	maxSequenceNumber uint64

	minSendTime int64
	maxSendTime int64

	minRecvTime int64 // for information only
	maxRecvTime int64 // for information only

	acked    classStat
	lost     classStat
	snBitmap *utils.Bitmap[uint64]

	aggregateSendDelta int64
	aggregateRecvDelta int64
	queuingDelay       int64

	isFinalized bool
}

func newPacketGroup(params packetGroupParams, queuingDelay int64) *packetGroup {
	return &packetGroup{
		params:       params,
		queuingDelay: queuingDelay,
		snBitmap:     utils.NewBitmap[uint64](params.Config.MinPackets),
	}
}

func (p *packetGroup) Add(pi *packetInfo, sendDelta, recvDelta int64, isLost bool) error {
	if isLost {
		return p.lostPacket(pi)
	}

	if err := p.inGroup(pi.sequenceNumber); err != nil {
		return err
	}

	if p.minSequenceNumber == 0 || pi.sequenceNumber < p.minSequenceNumber {
		p.minSequenceNumber = pi.sequenceNumber
	}
	p.maxSequenceNumber = max(p.maxSequenceNumber, pi.sequenceNumber)

	if p.minSendTime == 0 || (pi.sendTime-sendDelta) < p.minSendTime {
		p.minSendTime = pi.sendTime - sendDelta
	}
	p.maxSendTime = max(p.maxSendTime, pi.sendTime)

	if p.minRecvTime == 0 || (pi.recvTime-recvDelta) < p.minRecvTime {
		p.minRecvTime = pi.recvTime - recvDelta
	}
	p.maxRecvTime = max(p.maxRecvTime, pi.recvTime)

	p.acked.add(int(pi.size), pi.isRTX)
	if p.snBitmap.IsSet(pi.sequenceNumber - p.minSequenceNumber) {
		// an earlier packet reported as lost has been received
		p.snBitmap.Clear(pi.sequenceNumber - p.minSequenceNumber)
		p.lost.remove(int(pi.size), pi.isRTX)
	}

	// note that out-of-order deliveries will amplify the queueing delay.
	// for e.g. a, b, c getting delivered as a, c, b.
	// let us say packets are delivered with interval of `x`
	//   send delta aggregate will go up by x((a, c) = 2x + (c, b) -1x)
	//   recv delta aggregate will go up by 3x((a, c) = 2x + (c, b) 1x)
	p.aggregateSendDelta += sendDelta
	p.aggregateRecvDelta += recvDelta

	if p.acked.numPackets() == p.params.Config.MinPackets || (pi.sendTime-p.minSendTime) > p.params.Config.MaxWindowDuration.Microseconds() {
		p.isFinalized = true
	}
	return nil
}

func (p *packetGroup) lostPacket(pi *packetInfo) error {
	if pi.recvTime != 0 {
		// previously received packet, so not lost
		return nil
	}

	if err := p.inGroup(pi.sequenceNumber); err != nil {
		return err
	}

	if p.minSequenceNumber == 0 || pi.sequenceNumber < p.minSequenceNumber {
		p.minSequenceNumber = pi.sequenceNumber
	}
	p.maxSequenceNumber = max(p.maxSequenceNumber, pi.sequenceNumber)
	p.snBitmap.Set(pi.sequenceNumber - p.minSequenceNumber)

	p.lost.add(int(pi.size), pi.isRTX)
	return nil
}

func (p *packetGroup) MinSendTime() int64 {
	return p.minSendTime
}

func (p *packetGroup) PropagatedQueuingDelay() (int64, bool) {
	if !p.isFinalized {
		return 0, false
	}

	if p.queuingDelay+p.aggregateRecvDelta-p.aggregateSendDelta > 0 {
		return p.queuingDelay + p.aggregateRecvDelta - p.aggregateSendDelta, true
	}

	return max(0, p.aggregateRecvDelta-p.aggregateSendDelta), true
}

func (p *packetGroup) SendDuration() int64 {
	if !p.isFinalized {
		return 0
	}

	return p.maxSendTime - p.minSendTime
}

func (p *packetGroup) CapturedTrafficRatio() float64 {
	capturedTrafficRatio := float64(0.0)
	if p.aggregateRecvDelta != 0 {
		// apply a penalty for lost packets,
		// tha rationale being packet dropping is a strategy to relieve congestion
		// and if they were not dropped, they would have increased queuing delay,
		// as it is not possible to know the reason for the losses,
		// apply a small penalty to receive delta aggregate to simulate those packets
		// build up queuing delay.
		//
		// note that it is applied only for determining rate and
		// not while determining queuing region, adding synthetic delays
		// like this could cause queuing region to be stuck in JQR
		capturedTrafficRatio = float64(p.aggregateSendDelta) / float64(p.aggregateRecvDelta+p.getLossPenalty())
	}
	return min(1.0, capturedTrafficRatio)
}

func (p *packetGroup) Traffic() (int64, int64, int, float64) {
	numBytes := int(float64(p.acked.numBytes()) * p.CapturedTrafficRatio())

	fullness := max(
		float64(p.acked.numPackets())/float64(p.params.Config.MinPackets),
		float64(p.maxSendTime-p.minSendTime)/float64(p.params.Config.MaxWindowDuration.Microseconds()),
	)

	return p.minSendTime, p.maxSendTime - p.minSendTime, numBytes, fullness
}

func (p *packetGroup) WeightedLossRatio() (float64, bool) {
	if !p.isFinalized {
		return 0.0, false
	}

	return p.weightedLossRatio()
}

func (p *packetGroup) weightedLossRatio() (float64, bool) {
	lostPackets := p.lost.numPackets()
	totalPackets := float64(lostPackets + p.acked.numPackets())
	lossRatio := float64(0.0)
	if totalPackets != 0 {
		lossRatio = float64(lostPackets) / totalPackets
	}

	// Log10 is used to give higher weight for the same loss ratio at higher packet rates,
	// for e.g. with a penalty factor of 0.25
	//    - 10% loss at 20 total packets = 0.1 * log10(20) * 0.25 = 0.032
	//    - 10% loss at 100 total packets = 0.1 * log10(100) * 0.25 = 0.05
	//    - 10% loss at 1000 total packets = 0.1 * log10(100) * 0.25 = 0.075

	// indeterminate if not enough packets, the second return value will be false if indeterminate
	return lossRatio * math.Log10(totalPackets), totalPackets >= float64(p.params.Config.MinPackets)*p.params.Config.LossPenaltyMinPacketsRatio
}

func (p *packetGroup) MarshalLogObject(e zapcore.ObjectEncoder) error {
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

	e.AddUint64("minSequenceNumber", p.minSequenceNumber)
	e.AddUint64("maxSequenceNumber", p.maxSequenceNumber)
	e.AddObject("acked", p.acked)
	e.AddObject("lost", p.lost)

	sendBitrate := float64(0)
	if sendDuration != 0 {
		sendBitrate = float64(p.acked.numBytes()*8) / sendDuration.Seconds()
		e.AddFloat64("sendBitrate", sendBitrate)
	}

	recvBitrate := float64(0)
	if recvDuration != 0 {
		recvBitrate = float64(p.acked.numBytes()*8) / recvDuration.Seconds()
		e.AddFloat64("recvBitrate", recvBitrate)
	}

	e.AddInt64("aggregateSendDelta", p.aggregateSendDelta)
	e.AddInt64("aggregateRecvDelta", p.aggregateRecvDelta)
	e.AddInt64("queuingDelay", p.queuingDelay)
	e.AddInt64("groupDelay", p.aggregateRecvDelta-p.aggregateSendDelta)

	weightedLossRatio, weightedLossRatioValid := p.weightedLossRatio()
	e.AddFloat64("weightedLossRatio", weightedLossRatio)
	e.AddBool("weightedLossRatioValid", weightedLossRatioValid)

	if p.acked.numPackets() != 0 || p.lost.numPackets() != 0 {
		e.AddFloat64("rawLossRatio", float64(p.lost.numPackets())/float64(p.acked.numPackets()+p.lost.numPackets()))
	}

	e.AddInt64("lossPenalty", p.getLossPenalty())

	capturedTrafficRatio := p.CapturedTrafficRatio()
	e.AddFloat64("capturedTrafficRatio", capturedTrafficRatio)
	e.AddFloat64("estimatedAvailableChannelCapacity", sendBitrate*capturedTrafficRatio)

	e.AddBool("isFinalized", p.isFinalized)
	return nil
}

func (p *packetGroup) inGroup(sequenceNumber uint64) error {
	if p.isFinalized && sequenceNumber > p.maxSequenceNumber {
		return errGroupFinalized
	}

	if sequenceNumber < p.minSequenceNumber {
		return errOldPacket
	}

	return nil
}

func (p *packetGroup) getLossPenalty() int64 {
	weightedLossRatio, _ := p.weightedLossRatio()
	return int64(float64(p.aggregateRecvDelta) * weightedLossRatio * p.params.Config.LossPenaltyFactor)
}
