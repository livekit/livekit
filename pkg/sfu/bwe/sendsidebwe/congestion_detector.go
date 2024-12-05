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

	"github.com/frostbyte73/core"
	"github.com/gammazero/deque"
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

	pqd, pqdOk := pg.PropagatedQueuingDelay()
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

	PeriodicCheckInterval          time.Duration               `yaml:"periodic_check_interval,omitempty"`
	PeriodicCheckIntervalCongested time.Duration               `yaml:"periodic_check_interval_congested,omitempty"`
	CongestedCTRTrend              ccutils.TrendDetectorConfig `yaml:"congested_ctr_trend,omitempty"`
	CongestedCTREpsilon            float64                     `yaml:"congested_ctr_epsilon,omitempty"`
}

var (
	defaultTrendDetectorConfigCongestedCTR = ccutils.TrendDetectorConfig{
		RequiredSamples:        5,
		RequiredSamplesMin:     2,
		DownwardTrendThreshold: -0.5,
		DownwardTrendMaxWait:   5 * time.Second,
		CollapseThreshold:      500 * time.Millisecond,
		ValidityWindow:         10 * time.Second,
	}

	DefaultCongestionDetectorConfig = CongestionDetectorConfig{
		PacketGroup:                      DefaultPacketGroupConfig,
		PacketGroupMaxAge:                15 * time.Second,
		JQRMinDelay:                      15 * time.Millisecond,
		DQRMaxDelay:                      5 * time.Millisecond,
		WeightedLoss:                     defaultWeightedLossConfig,
		CongestionMinLoss:                0.25,
		QueuingDelayEarlyWarning:         DefaultQueuingDelayEarlyWarningCongestionSignalConfig,
		LossEarlyWarning:                 DefaultLossEarlyWarningCongestionSignalConfig,
		EarlyWarningHangover:             500 * time.Millisecond,
		QueuingDelayCongested:            DefaultQueuingDelayCongestedCongestionSignalConfig,
		LossCongested:                    DefaultLossCongestedCongestionSignalConfig,
		CongestedHangover:                3 * time.Second,
		RateMeasurementWindowDurationMin: 800 * time.Millisecond,
		RateMeasurementWindowDurationMax: 2 * time.Second,
		PeriodicCheckInterval:            2 * time.Second,
		PeriodicCheckIntervalCongested:   200 * time.Millisecond,
		CongestedCTRTrend:                defaultTrendDetectorConfigCongestedCTR,
		CongestedCTREpsilon:              0.05,
	}
)

// -------------------------------------------------------------------------------

type feedbackReport struct {
	at     time.Time
	report *rtcp.TransportLayerCC
}

type congestionDetectorParams struct {
	Config CongestionDetectorConfig
	Logger logger.Logger
}

type congestionDetector struct {
	params congestionDetectorParams

	lock            sync.RWMutex
	feedbackReports deque.Deque[feedbackReport]

	*packetTracker
	twccFeedback *twccFeedback

	packetGroups []*packetGroup

	wake chan struct{}
	stop core.Fuse

	estimatedAvailableChannelCapacity int64
	congestionState                   bwe.CongestionState
	congestionStateSwitchedAt         time.Time
	congestedCTRTrend                 *ccutils.TrendDetector[float64]
	congestedTrafficStats             *trafficStats

	bweListener bwe.BWEListener
}

func newCongestionDetector(params congestionDetectorParams) *congestionDetector {
	c := &congestionDetector{
		params:                            params,
		packetTracker:                     newPacketTracker(packetTrackerParams{Logger: params.Logger}),
		twccFeedback:                      newTWCCFeedback(twccFeedbackParams{Logger: params.Logger}),
		wake:                              make(chan struct{}, 1),
		estimatedAvailableChannelCapacity: 100_000_000,
		congestionState:                   bwe.CongestionStateNone,
		congestionStateSwitchedAt:         mono.Now(),
	}

	c.feedbackReports.SetMinCapacity(3)

	go c.worker()
	return c
}

func (c *congestionDetector) Stop() {
	c.stop.Break()
}

func (c *congestionDetector) SetBWEListener(bweListener bwe.BWEListener) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.bweListener = bweListener
}

func (c *congestionDetector) getBWEListener() bwe.BWEListener {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.bweListener
}

func (c *congestionDetector) HandleTWCCFeedback(report *rtcp.TransportLayerCC) {
	c.lock.Lock()
	c.feedbackReports.PushBack(feedbackReport{mono.Now(), report})
	c.lock.Unlock()

	// notify worker of a new feedback
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *congestionDetector) CongestionState() bwe.CongestionState {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.congestionState
}

func (c *congestionDetector) ProbeClusterStarting(pci ccutils.ProbeClusterInfo) {
	/* RAJA-TODO
	r.lock.Lock()
	defer r.lock.Unlock()

	r.lastExpectedBandwidthUsage = int64(pci.Goal.ExpectedUsageBps)

	r.params.Logger.Debugw(
		"remote bwe: starting probe",
		"lastReceived", r.lastReceivedEstimate,
		"expectedBandwidthUsage", r.lastExpectedBandwidthUsage,
		"channel", r.channelObserver,
	)

	r.isInProbe = true
	r.newChannelObserver()
	*/
}

func (c *congestionDetector) ProbeClusterDone(_pci ccutils.ProbeClusterInfo) (bwe.ProbeSignal, int64, error) {
	/* RAjA-TODO
	r.lock.Lock()
	defer r.lock.Unlock()

	// switch to a non-probe channel observer on probe end,
	// reset congestion state to get a fresh trend
	pco := r.channelObserver
	probeCongestionState := r.congestionState

	r.isInProbe = false
	r.congestionState = bwe.CongestionStateNone
	r.newChannelObserver()

	r.params.Logger.Debugw(
		"remote bwe: probe done",
		"lastReceived", r.lastReceivedEstimate,
		"expectedBandwidthUsage", r.lastExpectedBandwidthUsage,
		"channel", pco,
		"isSignalValid", pco.HasEnoughEstimateSamples(),
	)

	if probeCongestionState != bwe.CongestionStateNone {
		return bwe.ProbeSignalCongesting, r.committedChannelCapacity
	}

	trend, _ := pco.GetTrend()
	if !pco.HasEnoughEstimateSamples() || trend == channelTrendNeutral {
		return bwe.ProbeSignalInconclusive, r.committedChannelCapacity
	}

	highestEstimate := pco.GetHighestEstimate()
	if highestEstimate > r.committedChannelCapacity {
		r.committedChannelCapacity = highestEstimate
	}
	return bwe.ProbeSignalClearing, r.committedChannelCapacity
	*/
	return bwe.ProbeSignalClearing, 0, nil
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

func (c *congestionDetector) congestionDetectionStateMachine() {
	state := c.congestionState
	newState := c.congestionState
	reason := ""

	earlyWarningTriggered, earlyWarningReason, congestedTriggered, congestedReason, oldestContributingGroup := c.isCongestionSignalTriggered()

	switch state {
	case bwe.CongestionStateNone:
		if congestedTriggered {
			c.params.Logger.Warnw("invalid congested state transition", nil, "from", state, "reason", congestedReason)
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
			c.params.Logger.Warnw("invalid congested state transition", nil, "from", state, "reason", congestedReason)
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
			c.params.Logger.Warnw("invalid congested state transition", nil, "from", state, "reason", congestedReason)
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
	if newState != state {
		c.updateCongestionState(newState, reason, oldestContributingGroup)
	}
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
}

func (c *congestionDetector) updateCTRTrend(pg *packetGroup) {
	if c.congestedCTRTrend == nil {
		return
	}

	// progressively keep increasing the window and make measurements over longer windows,
	// if congestion is not relieving, CTR will trend down
	c.congestedTrafficStats.Merge(pg.Traffic())
	ctr := c.congestedTrafficStats.CapturedTrafficRatio()

	// quantise CTR to filter out small changes
	c.congestedCTRTrend.AddValue(float64(int((ctr+(c.params.Config.CongestedCTREpsilon/2))/c.params.Config.CongestedCTREpsilon)) * c.params.Config.CongestedCTREpsilon)

	if c.congestedCTRTrend.GetDirection() != ccutils.TrendDirectionDownward {
		return
	}

	c.params.Logger.Infow("captured traffic ratio is trending downward", "channel", c.congestedCTRTrend)

	if bweListener := c.getBWEListener(); bweListener != nil {
		bweListener.OnCongestionStateChange(c.congestionState, c.estimatedAvailableChannelCapacity)
	}

	// reset to get new set of samples for next trend
	c.resetCTRTrend()
}

func (c *congestionDetector) estimateAvailableChannelCapacity() {
	if len(c.packetGroups) == 0 {
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
			"not enough data to estimate available channel capacity",
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
		"congestion state change",
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

	if bweListener := c.getBWEListener(); bweListener != nil {
		bweListener.OnCongestionStateChange(state, c.estimatedAvailableChannelCapacity)
	}

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

func (c *congestionDetector) processFeedbackReport(fbr feedbackReport) {
	recvRefTime, isOutOfOrder := c.twccFeedback.ProcessReport(fbr.report, fbr.at)
	if isOutOfOrder {
		c.params.Logger.Infow("received out-of-order feedback report")
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

		err := pg.Add(pi, sendDelta, recvDelta, isLost)
		if err == nil {
			return
		}

		if err == errGroupFinalized {
			// previous group ended, start a new group
			c.updateCTRTrend(pg)

			// SSBWE-REMOVE c.params.Logger.Infow("packet group done", "group", pg, "numGroups", len(c.packetGroups)) // SSBWE-REMOVE
			pqd, _ := pg.PropagatedQueuingDelay()
			pg = newPacketGroup(
				packetGroupParams{
					Config:       c.params.Config.PacketGroup,
					WeightedLoss: c.params.Config.WeightedLoss,
					Logger:       c.params.Logger,
				},
				pqd,
			)
			c.packetGroups = append(c.packetGroups, pg)

			if err = pg.Add(pi, sendDelta, recvDelta, isLost); err != nil {
				c.params.Logger.Warnw("could not add packet to new packet group", err, "packetInfo", pi, "packetGroup", pg)
			}
			return
		}

		// try an older group
		for idx := len(c.packetGroups) - 2; idx >= 0; idx-- {
			opg := c.packetGroups[idx]
			if err := opg.Add(pi, sendDelta, recvDelta, isLost); err == nil {
				return
			} else if err == errGroupFinalized {
				c.params.Logger.Infow("unexpected finalized group", "packetInfo", pi, "packetGroup", opg)
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
	sequenceNumber := fbr.report.BaseSequenceNumber
	endSequenceNumberExclusive := sequenceNumber + fbr.report.PacketStatusCount
	deltaIdx := 0
	for _, chunk := range fbr.report.PacketChunks {
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
					recvRefTime += fbr.report.RecvDeltas[deltaIdx].Delta
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
					recvRefTime += fbr.report.RecvDeltas[deltaIdx].Delta
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
	c.congestionDetectionStateMachine()
}

func (c *congestionDetector) worker() {
	ticker := time.NewTicker(c.params.Config.PeriodicCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.wake:
			for {
				c.lock.Lock()
				if c.feedbackReports.Len() == 0 {
					c.lock.Unlock()
					break
				}
				fbReport := c.feedbackReports.PopFront()
				c.lock.Unlock()

				c.processFeedbackReport(fbReport)
			}

			if c.congestionState == bwe.CongestionStateCongested {
				ticker.Reset(c.params.Config.PeriodicCheckIntervalCongested)
			} else {
				ticker.Reset(c.params.Config.PeriodicCheckInterval)
			}

		case <-ticker.C:
			c.prunePacketGroups()
			c.congestionDetectionStateMachine()

		case <-c.stop.Watch():
			return
		}
	}
}
