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
	DefaultQueuingDelayEarlyWarningCongestionSignalConfig = CongestionSignalConfig{
		MinNumberOfGroups: 1,
		MinDuration:       100 * time.Millisecond,
	}

	DefaultLossEarlyWarningCongestionSignalConfig = CongestionSignalConfig{
		MinNumberOfGroups: 2,
		MinDuration:       200 * time.Millisecond,
	}

	DefaultQueuingDelayCongestedCongestionSignalConfig = CongestionSignalConfig{
		MinNumberOfGroups: 3,
		MinDuration:       300 * time.Millisecond,
	}

	DefaultLossCongestedCongestionSignalConfig = CongestionSignalConfig{
		MinNumberOfGroups: 5,
		MinDuration:       600 * time.Millisecond,
	}
)

// -------------------------------------------------------------------------------

type ProbeSignalConfig struct {
	MinBytesRatio    float64 `yaml:"min_bytes_ratio,omitempty"`
	MinDurationRatio float64 `yaml:"min_duration_ratio,omitempty"`

	JQRMinDelay time.Duration `yaml:"jqr_min_delay,omitempty"`
	DQRMaxDelay time.Duration `yaml:"dqr_max_delay,omitempty"`

	WeightedLoss      WeightedLossConfig `yaml:"weighted_loss,omitempty"`
	CongestionMinLoss float64            `yaml:"congestion_min_loss,omitempty"`
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
	if pqd > p.JQRMinDelay.Microseconds() {
		return ccutils.ProbeSignalCongesting, ts.AcknowledgedBitrate()
	}

	if ts.WeightedLoss() > p.CongestionMinLoss {
		return ccutils.ProbeSignalCongesting, ts.AcknowledgedBitrate()
	}

	if pqd < p.DQRMaxDelay.Microseconds() {
		return ccutils.ProbeSignalNotCongesting, ts.AcknowledgedBitrate()
	}

	return ccutils.ProbeSignalInconclusive, ts.AcknowledgedBitrate()
}

var (
	DefaultProbeSignalConfig = ProbeSignalConfig{
		MinBytesRatio:    0.5,
		MinDurationRatio: 0.5,

		JQRMinDelay: 15 * time.Millisecond,
		DQRMaxDelay: 5 * time.Millisecond,

		WeightedLoss:      defaultWeightedLossConfig,
		CongestionMinLoss: 0.25,
	}
)

// -------------------------------------------------------------------------------

type qdMeasurement struct {
	earlyWarningConfig CongestionSignalConfig
	congestedConfig    CongestionSignalConfig
	jqrMin             int64
	dqrMax             int64

	numGroups   int
	minSendTime int64
	maxSendTime int64
	isSealed    bool
}

func newQdMeasurement(
	earlyWarningConfig CongestionSignalConfig,
	congestedConfig CongestionSignalConfig,
	jqrMin int64,
	dqrMax int64,
) *qdMeasurement {
	return &qdMeasurement{
		earlyWarningConfig: earlyWarningConfig,
		congestedConfig:    congestedConfig,
		jqrMin:             jqrMin,
		dqrMax:             dqrMax,
	}
}

func (q *qdMeasurement) ProcessPacketGroup(pg *packetGroup) {
	if q.isSealed {
		return
	}

	pqd, pqdOk := pg.FinalizedPropagatedQueuingDelay()
	if !pqdOk {
		return
	}

	if pqd < q.dqrMax {
		// a DQR breaks continuity
		q.isSealed = true
		return
	}

	if pqd > q.jqrMin {
		q.numGroups++
		minSendTime, maxSendTime := pg.SendWindow()
		if q.minSendTime == 0 || minSendTime < q.minSendTime {
			q.minSendTime = minSendTime
		}
		if maxSendTime > q.maxSendTime {
			q.maxSendTime = maxSendTime
		}
	}

	// can seal if congested config thresholds are met as they are longer
	if q.congestedConfig.IsTriggered(q.numGroups, q.maxSendTime-q.minSendTime) {
		q.isSealed = true
	}
}

func (q *qdMeasurement) IsSealed() bool {
	return q.isSealed
}

func (q *qdMeasurement) IsEarlyWarningTriggered() bool {
	return q.earlyWarningConfig.IsTriggered(q.numGroups, q.maxSendTime-q.minSendTime)
}

func (q *qdMeasurement) IsCongestedTriggered() bool {
	return q.congestedConfig.IsTriggered(q.numGroups, q.maxSendTime-q.minSendTime)
}

func (q *qdMeasurement) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if q == nil {
		return nil
	}

	e.AddInt("numGroups", q.numGroups)
	e.AddInt64("minSendTime", q.minSendTime)
	e.AddInt64("maxSendTime", q.maxSendTime)
	e.AddDuration("duration", time.Duration((q.maxSendTime-q.minSendTime)*1000))
	e.AddBool("isSealed", q.isSealed)
	e.AddBool("isEarlyWarningTriggered", q.IsEarlyWarningTriggered())
	e.AddBool("isCongestedTriggered", q.IsCongestedTriggered())
	return nil
}

// -------------------------------------------------------------------------------

type lossMeasurement struct {
	earlyWarningConfig CongestionSignalConfig
	congestedConfig    CongestionSignalConfig
	congestionMinLoss  float64

	numGroups int
	ts        *trafficStats

	earlyWarningWeightedLoss float64
	congestedWeightedLoss    float64

	isSealed bool
}

func newLossMeasurement(
	earlyWarningConfig CongestionSignalConfig,
	congestedConfig CongestionSignalConfig,
	weightedLossConfig WeightedLossConfig,
	congestionMinLoss float64,
	logger logger.Logger,
) *lossMeasurement {
	return &lossMeasurement{
		earlyWarningConfig: earlyWarningConfig,
		congestedConfig:    congestedConfig,
		congestionMinLoss:  congestionMinLoss,
		ts: newTrafficStats(trafficStatsParams{
			Config: weightedLossConfig,
			Logger: logger,
		}),
	}
}

func (l *lossMeasurement) ProcessPacketGroup(pg *packetGroup) {
	if l.isSealed {
		return
	}

	l.numGroups++
	l.ts.Merge(pg.Traffic())

	duration := l.ts.Duration()
	if l.earlyWarningConfig.IsTriggered(l.numGroups, duration) {
		l.earlyWarningWeightedLoss = l.ts.WeightedLoss()
	}
	if l.congestedConfig.IsTriggered(l.numGroups, duration) {
		l.congestedWeightedLoss = l.ts.WeightedLoss()
		l.isSealed = true // can seal if congested thresholds are satisfied as those should be higher
	}
}

func (l *lossMeasurement) IsSealed() bool {
	return l.isSealed
}

func (l *lossMeasurement) IsEarlyWarningTriggered() bool {
	return l.earlyWarningWeightedLoss > l.congestionMinLoss
}

func (l *lossMeasurement) IsCongestedTriggered() bool {
	return l.congestedWeightedLoss > l.congestionMinLoss
}

func (l *lossMeasurement) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if l == nil {
		return nil
	}

	e.AddInt("numGroups", l.numGroups)
	e.AddObject("ts", l.ts)
	e.AddFloat64("earlyWarningWeightedLoss", l.earlyWarningWeightedLoss)
	e.AddFloat64("congestedWeightedLoss", l.congestedWeightedLoss)
	e.AddBool("isSealed", l.isSealed)
	e.AddBool("isEarlyWarningTriggered", l.IsEarlyWarningTriggered())
	e.AddBool("isCongestedTriggered", l.IsCongestedTriggered())
	return nil
}

// -------------------------------------------------------------------------------

type CongestionDetectorConfig struct {
	PacketGroup       PacketGroupConfig `yaml:"packet_group,omitempty"`
	PacketGroupMaxAge time.Duration     `yaml:"packet_group_max_age,omitempty"`

	ProbePacketGroup ProbePacketGroupConfig       `yaml:"probe_packet_group,omitempty"`
	ProbeRegulator   ccutils.ProbeRegulatorConfig `yaml:"probe_regulator,omitempty"`
	ProbeSignal      ProbeSignalConfig            `yaml:"probe_signal,omitempty"`

	JQRMinDelay time.Duration `yaml:"jqr_min_delay,omitempty"`
	DQRMaxDelay time.Duration `yaml:"dqr_max_delay,omitempty"`

	WeightedLoss      WeightedLossConfig `yaml:"weighted_loss,omitempty"`
	CongestionMinLoss float64            `yaml:"congestion_min_loss,omitempty"`

	QueuingDelayEarlyWarning CongestionSignalConfig `yaml:"queuing_delay_early_warning,omitempty"`
	LossEarlyWarning         CongestionSignalConfig `yaml:"loss_early_warning,omitempty"`
	EarlyWarningHangover     time.Duration          `yaml:"early_warning_hangover,omitempty"`

	QueuingDelayCongested CongestionSignalConfig `yaml:"queuing_delay_congested,omitempty"`
	LossCongested         CongestionSignalConfig `yaml:"loss_congested,omitempty"`
	CongestedHangover     time.Duration          `yaml:"congested_hangover,omitempty"`

	RateMeasurementWindowDurationMin time.Duration `yaml:"rate_measurement_window_duration_min,omitempty"`
	RateMeasurementWindowDurationMax time.Duration `yaml:"rate_measurement_window_duration_max,omitempty"`

	CongestedCTRTrend    ccutils.TrendDetectorConfig `yaml:"congested_ctr_trend,omitempty"`
	CongestedCTREpsilon  float64                     `yaml:"congested_ctr_epsilon,omitempty"`
	CongestedPacketGroup PacketGroupConfig           `yaml:"congested_packet_group,omitempty"`
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
		MinPackets:        8,
		MaxWindowDuration: 150 * time.Millisecond,
	}

	DefaultCongestionDetectorConfig = CongestionDetectorConfig{
		PacketGroup:       DefaultPacketGroupConfig,
		PacketGroupMaxAge: 10 * time.Second,

		ProbePacketGroup: DefaultProbePacketGroupConfig,
		ProbeRegulator:   ccutils.DefaultProbeRegulatorConfig,
		ProbeSignal:      DefaultProbeSignalConfig,

		JQRMinDelay: 15 * time.Millisecond,
		DQRMaxDelay: 5 * time.Millisecond,

		WeightedLoss:      defaultWeightedLossConfig,
		CongestionMinLoss: 0.25,

		QueuingDelayEarlyWarning: DefaultQueuingDelayEarlyWarningCongestionSignalConfig,
		LossEarlyWarning:         DefaultLossEarlyWarningCongestionSignalConfig,
		EarlyWarningHangover:     500 * time.Millisecond,

		QueuingDelayCongested: DefaultQueuingDelayCongestedCongestionSignalConfig,
		LossCongested:         DefaultLossCongestedCongestionSignalConfig,
		CongestedHangover:     3 * time.Second,

		RateMeasurementWindowDurationMin: 800 * time.Millisecond,
		RateMeasurementWindowDurationMax: 2 * time.Second,

		CongestedCTRTrend:    defaultTrendDetectorConfigCongestedCTR,
		CongestedCTREpsilon:  0.05,
		CongestedPacketGroup: defaultCongestedPacketGroupConfig,
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
	congestionState                   bwe.CongestionState
	congestionStateSwitchedAt         time.Time

	congestedCTRTrend     *ccutils.TrendDetector[float64]
	congestedTrafficStats *trafficStats
	congestedPacketGroup  *packetGroup

	bweListener bwe.BWEListener
}

func newCongestionDetector(params congestionDetectorParams) *congestionDetector {
	c := &congestionDetector{
		params:        params,
		rtt:           bwe.DefaultRTT,
		packetTracker: newPacketTracker(packetTrackerParams{Logger: params.Logger}),
		twccFeedback:  newTWCCFeedback(twccFeedbackParams{Logger: params.Logger}),
		probeRegulator: ccutils.NewProbeRegulator(ccutils.ProbeRegulatorParams{
			Config: params.Config.ProbeRegulator,
			Logger: params.Logger,
		}),
		estimatedAvailableChannelCapacity: 100_000_000,
		congestionState:                   bwe.CongestionStateNone,
		congestionStateSwitchedAt:         mono.Now(),
	}

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
	c.congestionState = bwe.CongestionStateNone
	c.congestionStateSwitchedAt = mono.Now()
	c.clearCTRTrend()
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

				recvTime := int64(0)
				isLost := false
				if chunk.PacketStatusSymbol != rtcp.TypeTCCPacketNotReceived {
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

		case *rtcp.StatusVectorChunk:
			for _, symbol := range chunk.SymbolList {
				if sequenceNumber == endSequenceNumberExclusive {
					break
				}

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
		}
	}

	c.prunePacketGroups()
	shouldNotify, state, committedChannelCapacity := c.congestionDetectionStateMachine()
	c.lock.Unlock()

	if shouldNotify {
		if bweListener := c.getBWEListener(); bweListener != nil {
			bweListener.OnCongestionStateChange(state, committedChannelCapacity)
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
	c.params.Logger.Debugw(
		"send side bwe: probe finalized",
		"isSignalValid", isSignalValid,
		"probeClusterInfo", pci,
		"probePacketGroup", c.probePacketGroup,
	)

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

func (c *congestionDetector) isCongestionSignalTriggered() (bool, string, bool, string, int) {
	qdMeasurement := newQdMeasurement(
		c.params.Config.QueuingDelayEarlyWarning,
		c.params.Config.QueuingDelayCongested,
		c.params.Config.JQRMinDelay.Microseconds(),
		c.params.Config.DQRMaxDelay.Microseconds(),
	)
	lossMeasurement := newLossMeasurement(
		c.params.Config.LossEarlyWarning,
		c.params.Config.LossCongested,
		c.params.Config.WeightedLoss,
		c.params.Config.CongestionMinLoss,
		c.params.Logger,
	)

	var idx int
	for idx = len(c.packetGroups) - 1; idx >= 0; idx-- {
		pg := c.packetGroups[idx]
		qdMeasurement.ProcessPacketGroup(pg)
		lossMeasurement.ProcessPacketGroup(pg)

		// if both measurements have enough data to make a decision, stop processing groups
		if qdMeasurement.IsSealed() && lossMeasurement.IsSealed() {
			break
		}

		// if congested triggered, can stop as that is the longer duration check and also
		// the worst case check, i. e. if "congested" is triggered due to any condition,
		// there can be nothing else that can trigger
		if qdMeasurement.IsCongestedTriggered() || lossMeasurement.IsCongestedTriggered() {
			break
		}
	}

	earlyWarningReason := ""
	earlyWarningTriggered := qdMeasurement.IsEarlyWarningTriggered()
	if earlyWarningTriggered {
		earlyWarningReason = "queuing-delay"
	} else {
		earlyWarningTriggered = lossMeasurement.IsEarlyWarningTriggered()
		if earlyWarningTriggered {
			earlyWarningReason = "loss"
		}
	}

	congestedReason := ""
	congestedTriggered := qdMeasurement.IsCongestedTriggered()
	if congestedTriggered {
		congestedReason = "queuing-delay"
	} else {
		congestedTriggered = lossMeasurement.IsCongestedTriggered()
		if congestedTriggered {
			congestedReason = "loss"
		}
	}

	return earlyWarningTriggered, earlyWarningReason, congestedTriggered, congestedReason, max(0, idx)
}

func (c *congestionDetector) congestionDetectionStateMachine() (bool, bwe.CongestionState, int64) {
	state := c.congestionState
	newState := c.congestionState
	reason := ""

	earlyWarningTriggered, earlyWarningReason, congestedTriggered, congestedReason, oldestContributingGroup := c.isCongestionSignalTriggered()

	switch state {
	case bwe.CongestionStateNone:
		if congestedTriggered {
			c.params.Logger.Warnw("send side bwe: invalid congested state transition", nil, "from", state, "reason", congestedReason)
		}
		if earlyWarningTriggered {
			newState = bwe.CongestionStateEarlyWarning
			reason = earlyWarningReason
		}

	case bwe.CongestionStateEarlyWarning:
		if congestedTriggered {
			newState = bwe.CongestionStateCongested
			reason = congestedReason
		} else if !earlyWarningTriggered {
			newState = bwe.CongestionStateEarlyWarningHangover
		}

	case bwe.CongestionStateEarlyWarningHangover:
		if congestedTriggered {
			c.params.Logger.Warnw("send side bwe: invalid congested state transition", nil, "from", state, "reason", congestedReason)
		}
		if earlyWarningTriggered {
			newState = bwe.CongestionStateEarlyWarning
			reason = earlyWarningReason
		} else if time.Since(c.congestionStateSwitchedAt) >= c.params.Config.EarlyWarningHangover {
			newState = bwe.CongestionStateNone
		}

	case bwe.CongestionStateCongested:
		if !congestedTriggered {
			newState = bwe.CongestionStateCongestedHangover
		}

	case bwe.CongestionStateCongestedHangover:
		if congestedTriggered {
			c.params.Logger.Warnw("send side bwe: invalid congested state transition", nil, "from", state, "reason", congestedReason)
		}
		if earlyWarningTriggered {
			newState = bwe.CongestionStateEarlyWarning
			reason = earlyWarningReason
		} else if time.Since(c.congestionStateSwitchedAt) >= c.params.Config.CongestedHangover {
			newState = bwe.CongestionStateNone
		}
	}

	c.estimateAvailableChannelCapacity()

	// update after running the above estimate as state change callback includes the estimated available channel capacity
	shouldNotify := false
	if newState != state {
		c.updateCongestionState(newState, reason, oldestContributingGroup)
		shouldNotify = true
	}

	if c.congestedCTRTrend != nil && c.congestedCTRTrend.GetDirection() == ccutils.TrendDirectionDownward {
		shouldNotify = true

		congestedAckedBitrate := c.congestedTrafficStats.AcknowledgedBitrate()
		if congestedAckedBitrate < c.estimatedAvailableChannelCapacity {
			c.estimatedAvailableChannelCapacity = congestedAckedBitrate
		}
		c.params.Logger.Infow(
			"send side bwe: captured traffic ratio is trending downward",
			"channel", c.congestedCTRTrend,
			"trafficStats", c.congestedTrafficStats,
			"estimatedAvailableChannelCapacity", c.estimatedAvailableChannelCapacity,
		)

		// reset to get new set of samples for next trend
		c.resetCTRTrend()
	}

	return shouldNotify, c.congestionState, c.estimatedAvailableChannelCapacity
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
	c.congestedPacketGroup = nil
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
		ctr := c.congestedTrafficStats.CapturedTrafficRatio()

		// quantise CTR to filter out small changes
		c.congestedCTRTrend.AddValue(float64(int((ctr+(c.params.Config.CongestedCTREpsilon/2))/c.params.Config.CongestedCTREpsilon)) * c.params.Config.CongestedCTREpsilon)
		c.params.Logger.Debugw("RAJA updating ctr trend", "ts", c.congestedTrafficStats, "ctr", ctr, "trend", c.congestedCTRTrend.GetDirection(), "ctrTrend", c.congestedCTRTrend) // REMOVE

		c.congestedPacketGroup = newPacketGroup(
			packetGroupParams{
				Config:       c.params.Config.CongestedPacketGroup,
				WeightedLoss: c.params.Config.WeightedLoss,
				Logger:       c.params.Logger,
			},
			0,
		)
	}
}

// RAJA-TODO: change this to use contributing groups only
func (c *congestionDetector) estimateAvailableChannelCapacity() {
	if len(c.packetGroups) == 0 || c.congestedCTRTrend != nil || c.probePacketGroup != nil {
		return
	}

	threshold, ok := c.packetTracker.BaseSendTimeThreshold(c.params.Config.RateMeasurementWindowDurationMax.Microseconds())
	if !ok {
		return
	}

	agg := newTrafficStats(trafficStatsParams{
		Config: c.params.Config.WeightedLoss,
		Logger: c.params.Logger,
	})
	var idx int
	for idx = len(c.packetGroups) - 1; idx >= 0; idx-- {
		pg := c.packetGroups[idx]
		if mst := pg.MinSendTime(); mst != 0 && mst < threshold {
			break
		}

		agg.Merge(pg.Traffic())
	}

	if agg.Duration() < c.params.Config.RateMeasurementWindowDurationMin.Microseconds() {
		c.params.Logger.Infow(
			"send side bwe: not enough data to estimate available channel capacity",
			"duration", agg.Duration(),
			"numGroups", len(c.packetGroups),
			"oldestUsed", max(0, idx),
		)
		return
	}

	c.estimatedAvailableChannelCapacity = agg.AcknowledgedBitrate()
}

func (c *congestionDetector) updateCongestionState(state bwe.CongestionState, reason string, oldestContributingGroup int) {
	c.params.Logger.Infow(
		"send side bwe: congestion state change",
		"from", c.congestionState,
		"to", state,
		"reason", reason,
		"numPacketGroups", len(c.packetGroups),
		"numContributingGroups", len(c.packetGroups[oldestContributingGroup:]),
		"contributingGroups", logger.ObjectSlice(c.packetGroups[oldestContributingGroup:]),
		"estimatedAvailableChannelCapacity", c.estimatedAvailableChannelCapacity,
	)

	if state != c.congestionState {
		c.congestionStateSwitchedAt = mono.Now()
	}

	prevState := c.congestionState
	c.congestionState = state

	// when in congested state, monitor changes in captured traffic ratio (CTR)
	// to ensure allocations are in line with latest estimates, it is possible that
	// the estimate is incorrect when congestion starts and the allocation may be
	// sub-optimal and not enough to reduce/relieve congestion, by monitoing CTR
	// on a continuous basis allocations can be adjusted in the direction of
	// reducing/relieving congestion
	if state == bwe.CongestionStateCongested && prevState != bwe.CongestionStateCongested {
		c.resetCTRTrend()
	} else if state != bwe.CongestionStateCongested {
		c.clearCTRTrend()
	}
}
