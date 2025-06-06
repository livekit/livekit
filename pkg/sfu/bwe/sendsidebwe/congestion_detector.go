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
	"fmt"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
	"github.com/pion/rtcp"
	"go.uber.org/zap/zapcore"
)

// -------------------------------------------------------------------------------

type CongestionSignalConfig struct {
	MinNumberOfGroups int           `yaml:"min_number_of_groups,omitempty"`
	MinDuration       time.Duration `yaml:"min_duration,omitempty"`
}

func (c CongestionSignalConfig) IsTriggered(numGroups int, duration int64) bool {
	return numGroups >= c.MinNumberOfGroups && duration >= c.MinDuration.Microseconds()
}

var (
	defaultQueuingDelayEarlyWarningJQRConfig = CongestionSignalConfig{
		MinNumberOfGroups: 2,
		MinDuration:       200 * time.Millisecond,
	}

	defaultQueuingDelayEarlyWarningDQRConfig = CongestionSignalConfig{
		MinNumberOfGroups: 3,
		MinDuration:       300 * time.Millisecond,
	}

	defaultLossEarlyWarningJQRConfig = CongestionSignalConfig{
		MinNumberOfGroups: 3,
		MinDuration:       300 * time.Millisecond,
	}

	defaultLossEarlyWarningDQRConfig = CongestionSignalConfig{
		MinNumberOfGroups: 4,
		MinDuration:       400 * time.Millisecond,
	}

	defaultQueuingDelayCongestedJQRConfig = CongestionSignalConfig{
		MinNumberOfGroups: 4,
		MinDuration:       400 * time.Millisecond,
	}

	defaultQueuingDelayCongestedDQRConfig = CongestionSignalConfig{
		MinNumberOfGroups: 5,
		MinDuration:       500 * time.Millisecond,
	}

	defaultLossCongestedJQRConfig = CongestionSignalConfig{
		MinNumberOfGroups: 6,
		MinDuration:       600 * time.Millisecond,
	}

	defaultLossCongestedDQRConfig = CongestionSignalConfig{
		MinNumberOfGroups: 6,
		MinDuration:       600 * time.Millisecond,
	}
)

// -------------------------------------------------------------------------------

type ProbeSignalConfig struct {
	MinBytesRatio    float64 `yaml:"min_bytes_ratio,omitempty"`
	MinDurationRatio float64 `yaml:"min_duration_ratio,omitempty"`

	JQRMinDelay time.Duration `yaml:"jqr_min_delay,omitempty"`
	DQRMaxDelay time.Duration `yaml:"dqr_max_delay,omitempty"`

	WeightedLoss       WeightedLossConfig `yaml:"weighted_loss,omitempty"`
	JQRMinWeightedLoss float64            `yaml:"jqr_min_weighted_loss,omitempty"`
	DQRMaxWeightedLoss float64            `yaml:"dqr_max_weighted_loss,omitempty"`
}

func (p ProbeSignalConfig) IsValid(pci ccutils.ProbeClusterInfo) bool {
	return pci.Result.Bytes() > int(p.MinBytesRatio*float64(pci.Goal.DesiredBytes)) && pci.Result.Duration() > time.Duration(p.MinDurationRatio*float64(pci.Goal.Duration))
}

func (p ProbeSignalConfig) ProbeSignal(ppg *probePacketGroup) (ccutils.ProbeSignal, int64) {
	ts := newTrafficStats(trafficStatsParams{
		Config: p.WeightedLoss,
	})
	ts.Merge(ppg.Traffic())

	pqd := ppg.PropagatedQueuingDelay()
	if pqd > p.JQRMinDelay.Microseconds() || ts.WeightedLoss() > p.JQRMinWeightedLoss {
		return ccutils.ProbeSignalCongesting, ts.AcknowledgedBitrate()
	}

	if pqd < p.DQRMaxDelay.Microseconds() && ts.WeightedLoss() < p.DQRMaxWeightedLoss {
		return ccutils.ProbeSignalNotCongesting, ts.AcknowledgedBitrate()
	}

	return ccutils.ProbeSignalInconclusive, ts.AcknowledgedBitrate()
}

var (
	defaultProbeSignalConfig = ProbeSignalConfig{
		MinBytesRatio:    0.5,
		MinDurationRatio: 0.5,

		JQRMinDelay: 50 * time.Millisecond,
		DQRMaxDelay: 20 * time.Millisecond,

		WeightedLoss:       defaultWeightedLossConfig,
		JQRMinWeightedLoss: 0.25,
		DQRMaxWeightedLoss: 0.1,
	}
)

// -------------------------------------------------------------------------------

type queuingRegion int

const (
	queuingRegionDQR queuingRegion = iota
	queuingRegionIndeterminate
	queuingRegionJQR
)

func (q queuingRegion) String() string {
	switch q {
	case queuingRegionDQR:
		return "DQR"
	case queuingRegionIndeterminate:
		return "INDETERMINATE"
	case queuingRegionJQR:
		return "JQR"
	default:
		return fmt.Sprintf("%d", int(q))
	}
}

// -------------------------------------------------------------------------------

type congestionReason int

const (
	congestionReasonNone congestionReason = iota
	congestionReasonQueuingDelay
	congestionReasonLoss
)

func (c congestionReason) String() string {
	switch c {
	case congestionReasonNone:
		return "NONE"
	case congestionReasonQueuingDelay:
		return "QUEUING_DELAY"
	case congestionReasonLoss:
		return "LOSS"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

// -------------------------------------------------------------------------------

type qdMeasurement struct {
	jqrConfig              CongestionSignalConfig
	dqrConfig              CongestionSignalConfig
	jqrMinDelay            int64
	jqrMinTrendCoefficient float64
	dqrMaxDelay            int64

	numGroups               int
	numJQRGroups            int
	numDQRGroups            int
	minSendTime             int64
	maxSendTime             int64
	propagatedQueuingDelays []int64

	isSealed    bool
	minGroupIdx int
	maxGroupIdx int

	queuingRegion queuingRegion
}

func newQDMeasurement(jqrConfig CongestionSignalConfig, dqrConfig CongestionSignalConfig, jqrMinDelay int64, jqrMinTrendCoefficient float64, dqrMaxDelay int64) *qdMeasurement {
	return &qdMeasurement{
		jqrConfig:              jqrConfig,
		dqrConfig:              dqrConfig,
		jqrMinDelay:            jqrMinDelay,
		jqrMinTrendCoefficient: jqrMinTrendCoefficient,
		dqrMaxDelay:            dqrMaxDelay,
		queuingRegion:          queuingRegionIndeterminate,
	}
}

func (q *qdMeasurement) ProcessPacketGroup(pg *packetGroup, groupIdx int) {
	if q.isSealed {
		return
	}

	pqd, pqdOk := pg.FinalizedPropagatedQueuingDelay()
	if !pqdOk {
		return
	}

	q.numGroups++
	if q.minGroupIdx == 0 || q.minGroupIdx > groupIdx {
		q.minGroupIdx = groupIdx
	}
	q.maxGroupIdx = max(q.maxGroupIdx, groupIdx)

	minSendTime, maxSendTime := pg.SendWindow()
	if q.minSendTime == 0 || minSendTime < q.minSendTime {
		q.minSendTime = minSendTime
	}
	q.maxSendTime = max(q.maxSendTime, maxSendTime)

	q.propagatedQueuingDelays = append(q.propagatedQueuingDelays, pqd)

	switch {
	case pqd < q.dqrMaxDelay:
		q.numDQRGroups++
		if q.numJQRGroups > 0 {
			// broken continuity, seal
			q.isSealed = true
		} else if q.dqrConfig.IsTriggered(q.numDQRGroups, q.maxSendTime-q.minSendTime) {
			q.isSealed = true
			q.queuingRegion = queuingRegionDQR
		}

	case pqd > q.jqrMinDelay:
		q.numJQRGroups++
		if q.numDQRGroups > 0 {
			// broken continuity, seal
			q.isSealed = true
		} else if q.jqrConfig.IsTriggered(q.numJQRGroups, q.maxSendTime-q.minSendTime) && q.trendCoefficient() > q.jqrMinTrendCoefficient {
			q.isSealed = true
			q.queuingRegion = queuingRegionJQR
		}

	default:
		if q.numDQRGroups > 0 || q.numJQRGroups > 0 {
			// broken continuity, seal
			q.isSealed = true
		}
	}
}

func (q *qdMeasurement) IsSealed() bool {
	return q.isSealed
}

func (q *qdMeasurement) QueuingRegion() queuingRegion {
	return q.queuingRegion
}

func (q *qdMeasurement) GroupRange() (int, int) {
	return max(0, q.minGroupIdx), max(0, q.maxGroupIdx)
}

func (q *qdMeasurement) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if q == nil {
		return nil
	}

	e.AddInt("numGroups", q.numGroups)
	e.AddInt("numJQRGroups", q.numJQRGroups)
	e.AddInt("numDQRGroups", q.numDQRGroups)
	e.AddInt64("minSendTime", q.minSendTime)
	e.AddInt64("maxSendTime", q.maxSendTime)
	e.AddArray("propagatedQueuingDelays", logger.Int64Slice(q.propagatedQueuingDelays))
	e.AddFloat64("trendCoefficient", q.trendCoefficient())
	e.AddDuration("duration", time.Duration((q.maxSendTime-q.minSendTime)*1000))
	e.AddBool("isSealed", q.isSealed)
	e.AddInt("minGroupIdx", q.minGroupIdx)
	e.AddInt("maxGroupIdx", q.maxGroupIdx)
	e.AddString("queuingRegion", q.queuingRegion.String())
	return nil
}

func (q *qdMeasurement) trendCoefficient() float64 {
	concordantPairs := 0
	discordantPairs := 0

	// the packet groups are processed from newest to oldest,
	// so a concordant pair is when the value drops,
	// i. e. the propagated queuing delay is increasing if newer (earlier entity in slice) is higher than older (later entity in slice)
	for i := 0; i < len(q.propagatedQueuingDelays)-1; i++ {
		for j := i + 1; j < len(q.propagatedQueuingDelays); j++ {
			if q.propagatedQueuingDelays[i] > q.propagatedQueuingDelays[j] {
				concordantPairs++
			} else if q.propagatedQueuingDelays[i] < q.propagatedQueuingDelays[j] {
				discordantPairs++
			}
		}
	}

	if (concordantPairs + discordantPairs) == 0 {
		// if the min requirements is only one sample, trend calculation is not possible, declare highest trend value
		if len(q.propagatedQueuingDelays) == 1 {
			return 1.0
		}

		return 0.0
	}

	return (float64(concordantPairs) - float64(discordantPairs)) / (float64(concordantPairs) + float64(discordantPairs))
}

// -------------------------------------------------------------------------------

type lossMeasurement struct {
	jqrConfig  CongestionSignalConfig
	dqrConfig  CongestionSignalConfig
	jqrMinLoss float64
	dqrMaxLoss float64

	numGroups int
	ts        *trafficStats

	isJQRSealed bool
	isDQRSealed bool
	minGroupIdx int
	maxGroupIdx int

	weightedLoss float64

	queuingRegion queuingRegion
}

func newLossMeasurement(
	jqrConfig CongestionSignalConfig,
	dqrConfig CongestionSignalConfig,
	weightedLossConfig WeightedLossConfig,
	jqrMinLoss float64,
	dqrMaxLoss float64,
	logger logger.Logger,
) *lossMeasurement {
	return &lossMeasurement{
		jqrConfig:  jqrConfig,
		dqrConfig:  dqrConfig,
		jqrMinLoss: jqrMinLoss,
		dqrMaxLoss: dqrMaxLoss,
		ts: newTrafficStats(trafficStatsParams{
			Config: weightedLossConfig,
			Logger: logger,
		}),
		queuingRegion: queuingRegionIndeterminate,
	}
}

func (l *lossMeasurement) ProcessPacketGroup(pg *packetGroup, groupIdx int) {
	if (l.isJQRSealed && l.isDQRSealed) || !pg.IsFinalized() {
		return
	}

	l.numGroups++
	if l.minGroupIdx == 0 || l.minGroupIdx > groupIdx {
		l.minGroupIdx = groupIdx
	}
	l.maxGroupIdx = max(l.maxGroupIdx, groupIdx)

	l.ts.Merge(pg.Traffic())

	if !l.isJQRSealed && l.jqrConfig.IsTriggered(l.numGroups, l.ts.Duration()) {
		l.isJQRSealed = true

		weightedLoss := l.ts.WeightedLoss()
		if weightedLoss > l.jqrMinLoss {
			l.weightedLoss = weightedLoss
			l.queuingRegion = queuingRegionJQR
			l.isDQRSealed = true // seal DQR also as queuing region has been determined
			return
		}
	}

	if !l.isDQRSealed && l.dqrConfig.IsTriggered(l.numGroups, l.ts.Duration()) {
		l.isDQRSealed = true

		weightedLoss := l.ts.WeightedLoss()
		if weightedLoss < l.dqrMaxLoss {
			l.weightedLoss = weightedLoss
			l.queuingRegion = queuingRegionDQR
			l.isJQRSealed = true // seal JQR also as queuing region has been determined
			return
		}
	}
}

func (l *lossMeasurement) IsSealed() bool {
	return l.isJQRSealed && l.isDQRSealed
}

func (l *lossMeasurement) QueuingRegion() queuingRegion {
	return l.queuingRegion
}

func (l *lossMeasurement) GroupRange() (int, int) {
	return max(0, l.minGroupIdx), max(0, l.maxGroupIdx)
}

func (l *lossMeasurement) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if l == nil {
		return nil
	}

	e.AddInt("numGroups", l.numGroups)
	e.AddObject("ts", l.ts)
	e.AddBool("isJQRSealed", l.isJQRSealed)
	e.AddBool("isDQRSealed", l.isDQRSealed)
	e.AddInt("minGroupIdx", l.minGroupIdx)
	e.AddInt("maxGroupIdx", l.maxGroupIdx)
	e.AddFloat64("weightedLoss", l.weightedLoss)
	e.AddString("queuingRegion", l.queuingRegion.String())
	return nil
}

// -------------------------------------------------------------------------------

type CongestionDetectorConfig struct {
	PacketGroup       PacketGroupConfig `yaml:"packet_group,omitempty"`
	PacketGroupMaxAge time.Duration     `yaml:"packet_group_max_age,omitempty"`

	ProbePacketGroup ProbePacketGroupConfig       `yaml:"probe_packet_group,omitempty"`
	ProbeRegulator   ccutils.ProbeRegulatorConfig `yaml:"probe_regulator,omitempty"`
	ProbeSignal      ProbeSignalConfig            `yaml:"probe_signal,omitempty"`

	JQRMinDelay            time.Duration `yaml:"jqr_min_delay,omitempty"`
	JQRMinTrendCoefficient float64       `yaml:"jqr_min_trend_coefficient,omitempty"`
	DQRMaxDelay            time.Duration `yaml:"dqr_max_delay,omitempty"`

	WeightedLoss       WeightedLossConfig `yaml:"weighted_loss,omitempty"`
	JQRMinWeightedLoss float64            `yaml:"jqr_min_weighted_loss,omitempty"`
	DQRMaxWeightedLoss float64            `yaml:"dqr_max_weighted_loss,omitempty"`

	QueuingDelayEarlyWarningJQR CongestionSignalConfig `yaml:"queuing_delay_early_warning_jqr,omitempty"`
	QueuingDelayEarlyWarningDQR CongestionSignalConfig `yaml:"queuing_delay_early_warning_dqr,omitempty"`
	LossEarlyWarningJQR         CongestionSignalConfig `yaml:"loss_early_warning_jqr,omitempty"`
	LossEarlyWarningDQR         CongestionSignalConfig `yaml:"loss_early_warning_dqr,omitempty"`

	QueuingDelayCongestedJQR CongestionSignalConfig `yaml:"queuing_delay_congested_jqr,omitempty"`
	QueuingDelayCongestedDQR CongestionSignalConfig `yaml:"queuing_delay_congested_dqr,omitempty"`
	LossCongestedJQR         CongestionSignalConfig `yaml:"loss_congested_jqr,omitempty"`
	LossCongestedDQR         CongestionSignalConfig `yaml:"loss_congested_dqr,omitempty"`

	CongestedCTRTrend    ccutils.TrendDetectorConfig `yaml:"congested_ctr_trend,omitempty"`
	CongestedCTREpsilon  float64                     `yaml:"congested_ctr_epsilon,omitempty"`
	CongestedPacketGroup PacketGroupConfig           `yaml:"congested_packet_group,omitempty"`

	EstimationWindowDuration time.Duration `yaml:"estimaton_window_duration,omitempty"`
}

var (
	defaultTrendDetectorConfigCongestedCTR = ccutils.TrendDetectorConfig{
		RequiredSamples:        4,
		RequiredSamplesMin:     2,
		DownwardTrendThreshold: -0.5,
		DownwardTrendMaxWait:   2 * time.Second,
		CollapseThreshold:      500 * time.Millisecond,
		ValidityWindow:         10 * time.Second,
	}

	defaultCongestedPacketGroupConfig = PacketGroupConfig{
		MinPackets:        20,
		MaxWindowDuration: 150 * time.Millisecond,
	}

	defaultCongestionDetectorConfig = CongestionDetectorConfig{
		PacketGroup:       defaultPacketGroupConfig,
		PacketGroupMaxAge: 10 * time.Second,

		ProbePacketGroup: defaultProbePacketGroupConfig,
		ProbeRegulator:   ccutils.DefaultProbeRegulatorConfig,
		ProbeSignal:      defaultProbeSignalConfig,

		JQRMinDelay:            50 * time.Millisecond,
		JQRMinTrendCoefficient: 0.8,
		DQRMaxDelay:            20 * time.Millisecond,

		WeightedLoss:       defaultWeightedLossConfig,
		JQRMinWeightedLoss: 0.25,
		DQRMaxWeightedLoss: 0.1,

		QueuingDelayEarlyWarningJQR: defaultQueuingDelayEarlyWarningJQRConfig,
		QueuingDelayEarlyWarningDQR: defaultQueuingDelayEarlyWarningDQRConfig,
		LossEarlyWarningJQR:         defaultLossEarlyWarningJQRConfig,
		LossEarlyWarningDQR:         defaultLossEarlyWarningDQRConfig,

		QueuingDelayCongestedJQR: defaultQueuingDelayCongestedJQRConfig,
		QueuingDelayCongestedDQR: defaultQueuingDelayCongestedDQRConfig,
		LossCongestedJQR:         defaultLossCongestedJQRConfig,
		LossCongestedDQR:         defaultLossCongestedDQRConfig,

		CongestedCTRTrend:    defaultTrendDetectorConfigCongestedCTR,
		CongestedCTREpsilon:  0.05,
		CongestedPacketGroup: defaultCongestedPacketGroupConfig,

		EstimationWindowDuration: time.Second,
	}
)

// -------------------------------------------------------------------------------

type congestionDetectorParams struct {
	Config CongestionDetectorConfig
	Logger logger.Logger
}

type congestionDetector struct {
	params congestionDetectorParams

	lock sync.Mutex

	rtt float64

	*packetTracker
	twccFeedback *twccFeedback

	packetGroups []*packetGroup

	probePacketGroup *probePacketGroup
	probeRegulator   *ccutils.ProbeRegulator

	estimatedAvailableChannelCapacity int64
	estimateTrafficStats              *trafficStats

	congestionState           bwe.CongestionState
	congestionStateSwitchedAt time.Time

	congestedCTRTrend     *ccutils.TrendDetector[float64]
	congestedTrafficStats *trafficStats
	congestedPacketGroup  *packetGroup

	congestionReason congestionReason
	qdMeasurement    *qdMeasurement
	lossMeasurement  *lossMeasurement

	bweListener bwe.BWEListener
}

func newCongestionDetector(params congestionDetectorParams) *congestionDetector {
	c := &congestionDetector{
		params:        params,
		packetTracker: newPacketTracker(packetTrackerParams{Logger: params.Logger}),
		twccFeedback:  newTWCCFeedback(twccFeedbackParams{Logger: params.Logger}),
	}
	c.Reset()

	return c
}

func (c *congestionDetector) Reset() {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.rtt = bwe.DefaultRTT

	c.packetGroups = nil

	c.probePacketGroup = nil
	c.probeRegulator = ccutils.NewProbeRegulator(ccutils.ProbeRegulatorParams{
		Config: c.params.Config.ProbeRegulator,
		Logger: c.params.Logger,
	})

	c.estimatedAvailableChannelCapacity = 100_000_000
	c.estimateTrafficStats = nil

	c.congestionState = bwe.CongestionStateNone
	c.congestionStateSwitchedAt = mono.Now()

	c.clearCTRTrend()

	c.congestionReason = congestionReasonNone
	c.qdMeasurement = nil
	c.lossMeasurement = nil
}

func (c *congestionDetector) SetBWEListener(bweListener bwe.BWEListener) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.bweListener = bweListener
}

func (c *congestionDetector) getBWEListener() bwe.BWEListener {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.bweListener
}

func (c *congestionDetector) HandleTWCCFeedback(report *rtcp.TransportLayerCC) {
	c.lock.Lock()
	recvRefTime, isOutOfOrder := c.twccFeedback.ProcessReport(report, mono.Now())
	if isOutOfOrder {
		c.params.Logger.Infow("send side bwe: received out-of-order feedback report")
	}

	if len(c.packetGroups) == 0 {
		c.packetGroups = append(
			c.packetGroups,
			newPacketGroup(
				packetGroupParams{
					Config:       c.params.Config.PacketGroup,
					WeightedLoss: c.params.Config.WeightedLoss,
					Logger:       c.params.Logger,
				},
				0,
			),
		)
	}

	pg := c.packetGroups[len(c.packetGroups)-1]
	trackPacketGroup := func(pi *packetInfo, sendDelta, recvDelta int64, isLost bool) {
		if pi == nil {
			return
		}

		c.updateCTRTrend(pi, sendDelta, recvDelta, isLost)

		if c.probePacketGroup != nil {
			c.probePacketGroup.Add(pi, sendDelta, recvDelta, isLost)
		}

		err := pg.Add(pi, sendDelta, recvDelta, isLost)
		if err == nil {
			return
		}

		if err == errGroupFinalized {
			// previous group ended, start a new group
			pg = newPacketGroup(
				packetGroupParams{
					Config:       c.params.Config.PacketGroup,
					WeightedLoss: c.params.Config.WeightedLoss,
					Logger:       c.params.Logger,
				},
				pg.PropagatedQueuingDelay(),
			)
			c.packetGroups = append(c.packetGroups, pg)

			if err = pg.Add(pi, sendDelta, recvDelta, isLost); err != nil {
				c.params.Logger.Warnw("send side bwe: could not add packet to new packet group", err, "packetInfo", pi, "packetGroup", pg)
			}
			return
		}

		// try an older group
		for idx := len(c.packetGroups) - 2; idx >= 0; idx-- {
			opg := c.packetGroups[idx]
			if err := opg.Add(pi, sendDelta, recvDelta, isLost); err == nil {
				return
			} else if err == errGroupFinalized {
				c.params.Logger.Infow("send side bwe: unexpected finalized group", "packetInfo", pi, "packetGroup", opg)
			}
		}
	}

	// 1. go through the TWCC feedback report and record recive time as reported by remote
	// 2. process acknowledged packet and group them
	//
	// losses are not recorded if a feedback report is completely lost.
	// RFC recommends treating lost reports by ignoring packets that would have been in it.
	// -----------------------------------------------------------------------------------
	// | From a congestion control perspective, lost feedback messages are               |
	// | handled by ignoring packets which would have been reported as lost or           |
	// | received in the lost feedback messages.  This behavior is similar to            |
	// | how a lost RTCP receiver report is handled.                                     |
	// -----------------------------------------------------------------------------------
	// Reference: https://datatracker.ietf.org/doc/html/draft-holmer-rmcat-transport-wide-cc-extensions-01#page-4
	sequenceNumber := report.BaseSequenceNumber
	endSequenceNumberExclusive := sequenceNumber + report.PacketStatusCount
	deltaIdx := 0
	processSymbol := func(symbol uint16) {
		recvTime := int64(0)
		isLost := false
		if symbol != rtcp.TypeTCCPacketNotReceived {
			recvRefTime += report.RecvDeltas[deltaIdx].Delta
			deltaIdx++

			recvTime = recvRefTime
		} else {
			isLost = true
		}
		pi, sendDelta, recvDelta := c.packetTracker.RecordPacketIndicationFromRemote(sequenceNumber, recvTime)
		if pi.sendTime != 0 {
			trackPacketGroup(&pi, sendDelta, recvDelta, isLost)
		}
		sequenceNumber++
	}
	for _, chunk := range report.PacketChunks {
		if sequenceNumber == endSequenceNumberExclusive {
			break
		}

		switch chunk := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for i := uint16(0); i < chunk.RunLength; i++ {
				if sequenceNumber == endSequenceNumberExclusive {
					break
				}

				processSymbol(chunk.PacketStatusSymbol)
			}

		case *rtcp.StatusVectorChunk:
			for _, symbol := range chunk.SymbolList {
				if sequenceNumber == endSequenceNumberExclusive {
					break
				}

				processSymbol(symbol)
			}
		}
	}

	c.prunePacketGroups()
	shouldNotify, fromState, toState, committedChannelCapacity := c.congestionDetectionStateMachine()
	c.lock.Unlock()

	if shouldNotify {
		if bweListener := c.getBWEListener(); bweListener != nil {
			bweListener.OnCongestionStateChange(fromState, toState, committedChannelCapacity)
		}
	}
}

func (c *congestionDetector) UpdateRTT(rtt float64) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if rtt == 0 {
		c.rtt = bwe.DefaultRTT
	} else {
		if c.rtt == 0 {
			c.rtt = rtt
		} else {
			c.rtt = bwe.RTTSmoothingFactor*rtt + (1.0-bwe.RTTSmoothingFactor)*c.rtt
		}
	}
}

func (c *congestionDetector) CongestionState() bwe.CongestionState {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.congestionState
}

func (c *congestionDetector) CanProbe() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.congestionState == bwe.CongestionStateNone && c.probePacketGroup == nil && c.probeRegulator.CanProbe()
}

func (c *congestionDetector) ProbeDuration() time.Duration {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.probeRegulator.ProbeDuration()
}

func (c *congestionDetector) ProbeClusterStarting(pci ccutils.ProbeClusterInfo) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.probePacketGroup = newProbePacketGroup(
		probePacketGroupParams{
			Config:       c.params.Config.ProbePacketGroup,
			WeightedLoss: c.params.Config.WeightedLoss,
			Logger:       c.params.Logger,
		},
		pci,
	)

	c.packetTracker.ProbeClusterStarting(pci.Id)
}

func (c *congestionDetector) ProbeClusterDone(pci ccutils.ProbeClusterInfo) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.packetTracker.ProbeClusterDone(pci.Id)
	if c.probePacketGroup != nil {
		c.probePacketGroup.ProbeClusterDone(pci)
	}
}

func (c *congestionDetector) ProbeClusterIsGoalReached() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.probePacketGroup == nil || c.congestionState != bwe.CongestionStateNone {
		return false
	}

	pci := c.probePacketGroup.ProbeClusterInfo()
	if !c.params.Config.ProbeSignal.IsValid(pci) {
		return false
	}

	probeSignal, estimatedAvailableChannelCapacity := c.params.Config.ProbeSignal.ProbeSignal(c.probePacketGroup)
	return probeSignal != ccutils.ProbeSignalNotCongesting && estimatedAvailableChannelCapacity > int64(pci.Goal.DesiredBps)
}

func (c *congestionDetector) ProbeClusterFinalize() (ccutils.ProbeSignal, int64, bool) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.probePacketGroup == nil {
		return ccutils.ProbeSignalInconclusive, 0, false
	}

	pci, isFinalized := c.probePacketGroup.MaybeFinalizeProbe(c.packetTracker.ProbeMaxSequenceNumber(), c.rtt)
	if !isFinalized {
		return ccutils.ProbeSignalInconclusive, 0, isFinalized
	}

	isSignalValid := c.params.Config.ProbeSignal.IsValid(pci)
	c.params.Logger.Infow(
		"send side bwe: probe finalized",
		"isSignalValid", isSignalValid,
		"probeClusterInfo", pci,
		"probePacketGroup", c.probePacketGroup,
		"congestionState", c.congestionState,
		"rtt", c.rtt,
	)

	// if congestion signal changed during probe, defer to that signal
	if c.congestionState != bwe.CongestionStateNone {
		probeSignal := ccutils.ProbeSignalCongesting
		c.probeRegulator.ProbeSignal(probeSignal, pci.CreatedAt)
		c.probePacketGroup = nil
		return probeSignal, c.estimatedAvailableChannelCapacity, true
	}

	probeSignal, estimatedAvailableChannelCapacity := c.params.Config.ProbeSignal.ProbeSignal(c.probePacketGroup)
	if probeSignal == ccutils.ProbeSignalNotCongesting && estimatedAvailableChannelCapacity > c.estimatedAvailableChannelCapacity {
		c.estimatedAvailableChannelCapacity = estimatedAvailableChannelCapacity
	}

	c.probeRegulator.ProbeSignal(probeSignal, pci.CreatedAt)
	c.probePacketGroup = nil
	return probeSignal, c.estimatedAvailableChannelCapacity, true
}

func (c *congestionDetector) prunePacketGroups() {
	if len(c.packetGroups) == 0 {
		return
	}

	threshold, ok := c.packetTracker.BaseSendTimeThreshold(c.params.Config.PacketGroupMaxAge.Microseconds())
	if !ok {
		return
	}

	for idx, pg := range c.packetGroups {
		if mst := pg.MinSendTime(); mst > threshold {
			c.packetGroups = c.packetGroups[idx:]
			return
		}
	}
}

func (c *congestionDetector) updateCongestionSignal(
	qdJQRConfig CongestionSignalConfig,
	qdDQRConfig CongestionSignalConfig,
	lossJQRConfig CongestionSignalConfig,
	lossDQRConfig CongestionSignalConfig,
) queuingRegion {
	c.qdMeasurement = newQDMeasurement(
		qdJQRConfig,
		qdDQRConfig,
		c.params.Config.JQRMinDelay.Microseconds(),
		c.params.Config.JQRMinTrendCoefficient,
		c.params.Config.DQRMaxDelay.Microseconds(),
	)
	c.lossMeasurement = newLossMeasurement(
		lossJQRConfig,
		lossDQRConfig,
		c.params.Config.WeightedLoss,
		c.params.Config.JQRMinWeightedLoss,
		c.params.Config.DQRMaxWeightedLoss,
		c.params.Logger,
	)

	var idx int
	for idx = len(c.packetGroups) - 1; idx >= 0; idx-- {
		pg := c.packetGroups[idx]
		c.qdMeasurement.ProcessPacketGroup(pg, idx)
		c.lossMeasurement.ProcessPacketGroup(pg, idx)

		// if both measurements have enough data to make a decision, stop processing groups
		if c.qdMeasurement.IsSealed() && c.lossMeasurement.IsSealed() {
			break
		}
	}

	qr := queuingRegionIndeterminate
	qdQueuingRegion := c.qdMeasurement.QueuingRegion()
	lossQueuingRegion := c.lossMeasurement.QueuingRegion()
	switch {
	case qdQueuingRegion == queuingRegionJQR:
		qr = queuingRegionJQR
		c.congestionReason = congestionReasonQueuingDelay
	case lossQueuingRegion == queuingRegionJQR:
		qr = queuingRegionJQR
		c.congestionReason = congestionReasonLoss
	case qdQueuingRegion == queuingRegionDQR && lossQueuingRegion == queuingRegionDQR:
		qr = queuingRegionDQR
		c.congestionReason = congestionReasonNone
	}

	return qr
}

func (c *congestionDetector) updateEarlyWarningSignal() queuingRegion {
	return c.updateCongestionSignal(
		c.params.Config.QueuingDelayEarlyWarningJQR,
		c.params.Config.QueuingDelayEarlyWarningDQR,
		c.params.Config.LossEarlyWarningJQR,
		c.params.Config.LossEarlyWarningDQR,
	)
}

func (c *congestionDetector) updateCongestedSignal() queuingRegion {
	return c.updateCongestionSignal(
		c.params.Config.QueuingDelayCongestedJQR,
		c.params.Config.QueuingDelayCongestedDQR,
		c.params.Config.LossCongestedJQR,
		c.params.Config.LossCongestedDQR,
	)
}

func (c *congestionDetector) congestionDetectionStateMachine() (bool, bwe.CongestionState, bwe.CongestionState, int64) {
	fromState := c.congestionState
	toState := c.congestionState

	switch fromState {
	case bwe.CongestionStateNone:
		if c.updateEarlyWarningSignal() == queuingRegionJQR {
			toState = bwe.CongestionStateEarlyWarning
		}

	case bwe.CongestionStateEarlyWarning:
		if c.updateCongestedSignal() == queuingRegionJQR {
			toState = bwe.CongestionStateCongested
		} else if c.updateEarlyWarningSignal() == queuingRegionDQR {
			toState = bwe.CongestionStateNone
		}

	case bwe.CongestionStateCongested:
		if c.updateCongestedSignal() == queuingRegionDQR {
			toState = bwe.CongestionStateNone
		}
	}

	shouldNotify := false
	if toState != fromState {
		c.estimateAvailableChannelCapacity()
		fromState, toState = c.updateCongestionState(toState)
		shouldNotify = true
	}

	if c.congestedCTRTrend != nil && c.congestedCTRTrend.GetDirection() == ccutils.TrendDirectionDownward {
		congestedAckedBitrate := c.congestedTrafficStats.AcknowledgedBitrate()
		if congestedAckedBitrate < c.estimatedAvailableChannelCapacity {
			c.estimatedAvailableChannelCapacity = congestedAckedBitrate

			c.params.Logger.Infow(
				"send side bwe: captured traffic ratio is trending downward",
				"channel", c.congestedCTRTrend,
				"trafficStats", c.congestedTrafficStats,
				"estimatedAvailableChannelCapacity", c.estimatedAvailableChannelCapacity,
			)

			shouldNotify = true
		}

		// reset to get new set of samples for next trend
		c.resetCTRTrend()
	}

	return shouldNotify, fromState, toState, c.estimatedAvailableChannelCapacity
}

func (c *congestionDetector) createCTRTrend() {
	c.resetCTRTrend()
	c.congestedPacketGroup = nil
}

func (c *congestionDetector) resetCTRTrend() {
	c.congestedCTRTrend = ccutils.NewTrendDetector[float64](ccutils.TrendDetectorParams{
		Name:   "ssbwe-ctr",
		Logger: c.params.Logger,
		Config: c.params.Config.CongestedCTRTrend,
	})
	c.congestedTrafficStats = newTrafficStats(trafficStatsParams{
		Config: c.params.Config.WeightedLoss,
		Logger: c.params.Logger,
	})
}

func (c *congestionDetector) clearCTRTrend() {
	c.congestedCTRTrend = nil
	c.congestedTrafficStats = nil
	c.congestedPacketGroup = nil
}

func (c *congestionDetector) updateCTRTrend(pi *packetInfo, sendDelta, recvDelta int64, isLost bool) {
	if c.congestedCTRTrend == nil {
		return
	}

	if c.congestedPacketGroup == nil {
		c.congestedPacketGroup = newPacketGroup(
			packetGroupParams{
				Config:       c.params.Config.CongestedPacketGroup,
				WeightedLoss: c.params.Config.WeightedLoss,
				Logger:       c.params.Logger,
			},
			0,
		)
	}

	if err := c.congestedPacketGroup.Add(pi, sendDelta, recvDelta, isLost); err == errGroupFinalized {
		// progressively keep increasing the window and make measurements over longer windows,
		// if congestion is not relieving, CTR will trend down
		c.congestedTrafficStats.Merge(c.congestedPacketGroup.Traffic())

		ts := newTrafficStats(trafficStatsParams{
			Config: c.params.Config.WeightedLoss,
			Logger: c.params.Logger,
		})
		ts.Merge(c.congestedPacketGroup.Traffic())
		ctr := ts.CapturedTrafficRatio()

		// quantise CTR to filter out small changes
		c.congestedCTRTrend.AddValue(float64(int((ctr+(c.params.Config.CongestedCTREpsilon/2))/c.params.Config.CongestedCTREpsilon)) * c.params.Config.CongestedCTREpsilon)

		c.congestedPacketGroup = newPacketGroup(
			packetGroupParams{
				Config:       c.params.Config.CongestedPacketGroup,
				WeightedLoss: c.params.Config.WeightedLoss,
				Logger:       c.params.Logger,
			},
			c.congestedPacketGroup.PropagatedQueuingDelay(),
		)
	}
}

func (c *congestionDetector) estimateAvailableChannelCapacity() {
	c.estimateTrafficStats = nil
	if len(c.packetGroups) == 0 || c.probePacketGroup != nil {
		return
	}

	// when congested, use contributing groups,
	// else use a time windowed measurement
	useWindow := false
	isAggValid := true
	minGroupIdx := 0
	maxGroupIdx := len(c.packetGroups) - 1
	switch c.congestionReason {
	case congestionReasonQueuingDelay:
		minGroupIdx, maxGroupIdx = c.qdMeasurement.GroupRange()
	case congestionReasonLoss:
		minGroupIdx, maxGroupIdx = c.lossMeasurement.GroupRange()
	default:
		useWindow = true
		isAggValid = false
	}

	agg := newTrafficStats(trafficStatsParams{
		Config: c.params.Config.WeightedLoss,
		Logger: c.params.Logger,
	})
	for idx := maxGroupIdx; idx >= minGroupIdx; idx-- {
		pg := c.packetGroups[idx]
		if !pg.IsFinalized() {
			continue
		}

		agg.Merge(pg.Traffic())
		if useWindow && agg.Duration() > c.params.Config.EstimationWindowDuration.Microseconds() {
			isAggValid = true
			break
		}
	}

	if isAggValid {
		c.estimatedAvailableChannelCapacity = agg.AcknowledgedBitrate()
		c.estimateTrafficStats = agg
	}
}

func (c *congestionDetector) updateCongestionState(state bwe.CongestionState) (bwe.CongestionState, bwe.CongestionState) {
	loggingFields := []any{
		"from", c.congestionState,
		"to", state,
		"congestionReason", c.congestionReason,
		"qdMeasurement", c.qdMeasurement,
		"lossMeasurement", c.lossMeasurement,
		"numPacketGroups", len(c.packetGroups),
		"estimatedAvailableChannelCapacity", c.estimatedAvailableChannelCapacity,
		"estimateTrafficStats", c.estimateTrafficStats,
	}
	if c.congestionReason != congestionReasonNone {
		var minGroupIdx, maxGroupIdx int
		switch c.congestionReason {
		case congestionReasonQueuingDelay:
			minGroupIdx, maxGroupIdx = c.qdMeasurement.GroupRange()
		case congestionReasonLoss:
			minGroupIdx, maxGroupIdx = c.lossMeasurement.GroupRange()
		}
		loggingFields = append(
			loggingFields,
			"contributingGroups", logger.ObjectSlice(c.packetGroups[minGroupIdx:maxGroupIdx+1]),
		)
	}
	c.params.Logger.Infow("send side bwe: congestion state change", loggingFields...)

	if state != c.congestionState {
		c.congestionStateSwitchedAt = mono.Now()
	}

	fromState := c.congestionState
	c.congestionState = state

	// when in congested state, monitor changes in captured traffic ratio (CTR)
	// to ensure allocations are in line with latest estimates, it is possible that
	// the estimate is incorrect when congestion starts and the allocation may be
	// sub-optimal and not enough to reduce/relieve congestion, by monitoing CTR
	// on a continuous basis allocations can be adjusted in the direction of
	// reducing/relieving congestion
	if state == bwe.CongestionStateCongested && fromState != bwe.CongestionStateCongested {
		c.createCTRTrend()
	} else if state != bwe.CongestionStateCongested {
		c.clearCTRTrend()
	}

	return fromState, c.congestionState
}
