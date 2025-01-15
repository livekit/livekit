// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sendsidebwe

import (
	"errors"
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
}

var (
	defaultPacketGroupConfig = PacketGroupConfig{
		MinPackets:        30,
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
	probe   stat
}

func (c *classStat) add(size int, isRTX bool, isProbe bool) {
	if isRTX {
		c.rtx.add(size)
	} else if isProbe {
		c.probe.add(size)
	} else {
		c.primary.add(size)
	}
}

func (c *classStat) remove(size int, isRTX bool, isProbe bool) {
	if isRTX {
		c.rtx.remove(size)
	} else if isProbe {
		c.probe.remove(size)
	} else {
		c.primary.remove(size)
	}
}

func (c *classStat) numPackets() int {
	return c.primary.getNumPackets() + c.rtx.getNumPackets() + c.probe.getNumPackets()
}

func (c *classStat) numBytes() int {
	return c.primary.getNumBytes() + c.rtx.getNumBytes() + c.probe.getNumBytes()
}

func (c classStat) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddObject("primary", c.primary)
	e.AddObject("rtx", c.rtx)
	e.AddObject("probe", c.probe)
	return nil
}

// -------------------------------------------------------------

type packetGroupParams struct {
	Config       PacketGroupConfig
	WeightedLoss WeightedLossConfig
	Logger       logger.Logger
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

	aggregateSendDelta    int64
	aggregateRecvDelta    int64
	inheritedQueuingDelay int64

	isFinalized bool
}

func newPacketGroup(params packetGroupParams, inheritedQueuingDelay int64) *packetGroup {
	return &packetGroup{
		params:                params,
		inheritedQueuingDelay: inheritedQueuingDelay,
		snBitmap:              utils.NewBitmap[uint64](params.Config.MinPackets),
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

	p.acked.add(int(pi.size), pi.isRTX, pi.isProbe)
	if int(pi.sequenceNumber-p.minSequenceNumber) < p.snBitmap.Len() && p.snBitmap.IsSet(pi.sequenceNumber-p.minSequenceNumber) {
		// an earlier packet reported as lost has been received
		p.snBitmap.Clear(pi.sequenceNumber - p.minSequenceNumber)
		p.lost.remove(int(pi.size), pi.isRTX, pi.isProbe)
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

	p.lost.add(int(pi.size), pi.isRTX, pi.isProbe)
	return nil
}

func (p *packetGroup) MinSendTime() int64 {
	return p.minSendTime
}

func (p *packetGroup) SendWindow() (int64, int64) {
	return p.minSendTime, p.maxSendTime
}

func (p *packetGroup) PropagatedQueuingDelay() int64 {
	if p.inheritedQueuingDelay+p.aggregateRecvDelta-p.aggregateSendDelta > 0 {
		return p.inheritedQueuingDelay + p.aggregateRecvDelta - p.aggregateSendDelta
	}

	return max(0, p.aggregateRecvDelta-p.aggregateSendDelta)
}

func (p *packetGroup) FinalizedPropagatedQueuingDelay() (int64, bool) {
	if !p.isFinalized {
		return 0, false
	}

	return p.PropagatedQueuingDelay(), true
}

func (p *packetGroup) IsFinalized() bool {
	return p.isFinalized
}

func (p *packetGroup) Traffic() *trafficStats {
	return &trafficStats{
		minSendTime:  p.minSendTime,
		maxSendTime:  p.maxSendTime,
		sendDelta:    p.aggregateSendDelta,
		recvDelta:    p.aggregateRecvDelta,
		ackedPackets: p.acked.numPackets(),
		ackedBytes:   p.acked.numBytes(),
		lostPackets:  p.lost.numPackets(),
		lostBytes:    p.lost.numBytes(),
	}
}

func (p *packetGroup) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if p == nil {
		return nil
	}

	e.AddUint64("minSequenceNumber", p.minSequenceNumber)
	e.AddUint64("maxSequenceNumber", p.maxSequenceNumber)
	e.AddObject("acked", p.acked)
	e.AddObject("lost", p.lost)

	e.AddInt64("minRecvTime", p.minRecvTime)
	e.AddInt64("maxRecvTime", p.maxRecvTime)
	recvDuration := time.Duration((p.maxRecvTime - p.minRecvTime) * 1000)
	e.AddDuration("recvDuration", recvDuration)

	recvBitrate := float64(0)
	if recvDuration != 0 {
		recvBitrate = float64(p.acked.numBytes()*8) / recvDuration.Seconds()
		e.AddFloat64("recvBitrate", recvBitrate)
	}

	ts := newTrafficStats(trafficStatsParams{
		Config: p.params.WeightedLoss,
		Logger: p.params.Logger,
	})
	ts.Merge(p.Traffic())
	e.AddObject("trafficStats", ts)
	e.AddInt64("inheritedQueuingDelay", p.inheritedQueuingDelay)
	e.AddInt64("propagatedQueuingDelay", p.PropagatedQueuingDelay())

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
