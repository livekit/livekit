package sendsidebwe

import (
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/gammazero/deque"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
	"github.com/pion/rtcp"
)

// -------------------------------------------------------------------------------

type CongestionSignalConfig struct {
	MinNumberOfGroups int           `yaml:"min_number_of_groups,omitempty"`
	MinDuration       time.Duration `yaml:"min_duration,omitempty"`
}

var (
	DefaultEarlyWarningCongestionSignalConfig = CongestionSignalConfig{
		MinNumberOfGroups: 1,
		MinDuration:       100 * time.Millisecond,
	}

	DefaultCongestedCongestionSignalConfig = CongestionSignalConfig{
		MinNumberOfGroups: 3,
		MinDuration:       300 * time.Millisecond,
	}
)

// -------------------------------------------------------------------------------

type CongestionDetectorConfig struct {
	PacketGroup       PacketGroupConfig `yaml:"packet_group,omitempty"`
	PacketGroupMaxAge time.Duration     `yaml:"packet_group_max_age,omitempty"`

	JQRMinDelay time.Duration `yaml:"jqr_min_delay,omitempty"`
	DQRMaxDelay time.Duration `yaml:"dqr_max_delay,omitempty"`

	EarlyWarning         CongestionSignalConfig `yaml:"early_warning,omitempty"`
	EarlyWarningHangover time.Duration          `yaml:"early_warning_hangover,omitempty"`
	Congested            CongestionSignalConfig `yaml:"congested,omitempty"`
	CongestedHangover    time.Duration          `yaml:"congested_hangover,omitempty"`

	RateMeasurementWindowFullnessMin float64       `yaml:"rate_measurement_window_fullness_min,omitempty"`
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
		DownwardTrendThreshold: -0.6,
		DownwardTrendMaxWait:   5 * time.Second,
		CollapseThreshold:      500 * time.Millisecond,
		ValidityWindow:         10 * time.Second,
	}

	DefaultCongestionDetectorConfig = CongestionDetectorConfig{
		PacketGroup:                      DefaultPacketGroupConfig,
		PacketGroupMaxAge:                30 * time.Second,
		JQRMinDelay:                      15 * time.Millisecond,
		DQRMaxDelay:                      5 * time.Millisecond,
		EarlyWarning:                     DefaultEarlyWarningCongestionSignalConfig,
		EarlyWarningHangover:             500 * time.Millisecond,
		Congested:                        DefaultCongestedCongestionSignalConfig,
		CongestedHangover:                3 * time.Second,
		RateMeasurementWindowFullnessMin: 0.8,
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
	congestionState                   CongestionState
	congestionStateSwitchedAt         time.Time
	congestedCTRTrend                 *ccutils.TrendDetector[float64]

	onCongestionStateChange func(congestionState CongestionState, estimatedAvailableChannelCapacity int64)

	probeGroup *packetGroup
}

func newCongestionDetector(params congestionDetectorParams) *congestionDetector {
	c := &congestionDetector{
		params:                            params,
		packetTracker:                     newPacketTracker(packetTrackerParams{Logger: params.Logger}),
		twccFeedback:                      newTWCCFeedback(twccFeedbackParams{Logger: params.Logger}),
		wake:                              make(chan struct{}, 1),
		estimatedAvailableChannelCapacity: 100_000_000,
	}

	c.feedbackReports.SetMinCapacity(3)

	go c.worker()
	return c
}

func (c *congestionDetector) Stop() {
	c.stop.Once(func() {
		close(c.wake)
	})
}

func (c *congestionDetector) OnCongestionStateChange(f func(congestionState CongestionState, estimatedAvailableChannelCapacity int64)) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.onCongestionStateChange = f
}

func (c *congestionDetector) GetCongestionState() CongestionState {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.congestionState
}

func (c *congestionDetector) updateCongestionState(state CongestionState) {
	c.lock.Lock()
	c.params.Logger.Infow(
		"congestion state change",
		"from", c.congestionState,
		"to", state,
		"estimatedAvailableChannelCapacity", c.estimatedAvailableChannelCapacity,
	)

	prevState := c.congestionState
	c.congestionState = state

	onCongestionStateChange := c.onCongestionStateChange
	estimatedAvailableChannelCapacity := c.estimatedAvailableChannelCapacity
	c.lock.Unlock()

	if onCongestionStateChange != nil {
		onCongestionStateChange(state, estimatedAvailableChannelCapacity)
	}

	// when in congested state, monitor changes in captured traffic ratio (CTR)
	// to ensure allocations are in line with latest estimates, it is possible that
	// the estimate is incorrect when congestion starts and the allocation may be
	// sub-optimal and not enough to reduce/relieve congestion, by monitoing CTR
	// on a continuous basis allocations can be adjusted in the direction of
	// reducing/relieving congestion
	if state == CongestionStateCongested && prevState != CongestionStateCongested {
		c.congestedCTRTrend = ccutils.NewTrendDetector[float64](ccutils.TrendDetectorParams{
			Name:   "ssbwe-estimate",
			Logger: c.params.Logger,
			Config: c.params.Config.CongestedCTRTrend,
		})
	} else if state != CongestionStateCongested {
		c.congestedCTRTrend = nil
	}
}

func (c *congestionDetector) GetEstimatedAvailableChannelCapacity() int64 {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.estimatedAvailableChannelCapacity
}

func (c *congestionDetector) HandleRTCP(report *rtcp.TransportLayerCC) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.stop.IsBroken() {
		return
	}

	c.feedbackReports.PushBack(feedbackReport{mono.Now(), report})

	// notify worker of a new feedback
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *congestionDetector) prunePacketGroups() {
	if len(c.packetGroups) == 0 {
		return
	}

	threshold := c.packetGroups[len(c.packetGroups)-1].MinSendTime() - c.params.Config.PacketGroupMaxAge.Microseconds()
	for idx, pg := range c.packetGroups {
		if mst := pg.MinSendTime(); mst < threshold {
			c.packetGroups = c.packetGroups[idx+1:]
			return
		}
	}
}

func (c *congestionDetector) isCongestionSignalTriggered() (bool, bool) {
	earlyWarningTriggered := false
	congestedTriggered := false

	numGroups := 0
	duration := int64(0)
	for idx := len(c.packetGroups) - 1; idx >= 0; idx-- {
		pg := c.packetGroups[idx]
		pqd, ok := pg.PropagatedQueuingDelay()
		if !ok {
			continue
		}

		if pqd > c.params.Config.JQRMinDelay.Microseconds() {
			// JQR group builds up congestion signal
			numGroups++
			duration += pg.SendDuration()
		}

		// INDETERMINATE group is treated as a no-op

		if pqd < c.params.Config.DQRMaxDelay.Microseconds() {
			// any DQR group breaks the continuity
			return earlyWarningTriggered, congestedTriggered
		}

		if numGroups >= c.params.Config.EarlyWarning.MinNumberOfGroups && duration >= c.params.Config.EarlyWarning.MinDuration.Microseconds() {
			earlyWarningTriggered = true
		}
		if numGroups >= c.params.Config.Congested.MinNumberOfGroups && duration >= c.params.Config.Congested.MinDuration.Microseconds() {
			congestedTriggered = true
		}
		if earlyWarningTriggered && congestedTriggered {
			break
		}
	}

	return earlyWarningTriggered, congestedTriggered
}

func (c *congestionDetector) congestionDetectionStateMachine() {
	state := c.GetCongestionState()
	newState := state

	earlyWarningTriggered, congestedTriggered := c.isCongestionSignalTriggered()

	switch state {
	case CongestionStateNone:
		if congestedTriggered {
			c.params.Logger.Warnw("invalid congested state transition", nil, "from", state)
		}
		if earlyWarningTriggered {
			newState = CongestionStateEarlyWarning
		}

	case CongestionStateEarlyWarning:
		if congestedTriggered {
			newState = CongestionStateCongested
		} else if !earlyWarningTriggered {
			newState = CongestionStateEarlyWarningHangover
		}

	case CongestionStateEarlyWarningHangover:
		if congestedTriggered {
			c.params.Logger.Warnw("invalid congested state transition", nil, "from", state)
		}
		if earlyWarningTriggered {
			newState = CongestionStateEarlyWarning
		} else if time.Since(c.congestionStateSwitchedAt) >= c.params.Config.EarlyWarningHangover {
			newState = CongestionStateNone
		}

	case CongestionStateCongested:
		if !congestedTriggered {
			newState = CongestionStateCongestedHangover
		}

	case CongestionStateCongestedHangover:
		if congestedTriggered {
			c.params.Logger.Warnw("invalid congested state transition", nil, "from", state)
		}
		if earlyWarningTriggered {
			newState = CongestionStateEarlyWarning
		} else if time.Since(c.congestionStateSwitchedAt) >= c.params.Config.CongestedHangover {
			newState = CongestionStateNone
		}
	}

	c.estimateAvailableChannelCapacity()

	// update after running the above estimate as state change callback includes the estimated available channel capacity
	if newState != state {
		c.congestionStateSwitchedAt = mono.Now()
		c.updateCongestionState(newState)
	}
}

func (c *congestionDetector) updateTrend(ctr float64) {
	if c.congestedCTRTrend == nil {
		return
	}

	// quantise the CTR to filter out small changes
	c.congestedCTRTrend.AddValue(float64(int((ctr+(c.params.Config.CongestedCTREpsilon/2))/c.params.Config.CongestedCTREpsilon)) * c.params.Config.CongestedCTREpsilon)

	if c.congestedCTRTrend.GetDirection() == ccutils.TrendDirectionDownward {
		c.params.Logger.Infow("captured traffic ratio is trending downward", "channel", c.congestedCTRTrend.ToString())

		c.lock.RLock()
		state := c.congestionState
		estimatedAvailableChannelCapacity := c.estimatedAvailableChannelCapacity
		onCongestionStateChange := c.onCongestionStateChange
		c.lock.RUnlock()

		if onCongestionStateChange != nil {
			onCongestionStateChange(state, estimatedAvailableChannelCapacity)
		}

		// reset to get new set of samples for next trend
		c.congestedCTRTrend = ccutils.NewTrendDetector[float64](ccutils.TrendDetectorParams{
			Name:   "ssbwe-estimate",
			Logger: c.params.Logger,
			Config: c.params.Config.CongestedCTRTrend,
		})
	}
}

func (c *congestionDetector) estimateAvailableChannelCapacity() {
	if len(c.packetGroups) == 0 {
		return
	}

	totalDuration := int64(0)
	totalBytes := 0
	threshold := c.packetGroups[len(c.packetGroups)-1].MinSendTime() - c.params.Config.RateMeasurementWindowDurationMax.Microseconds()
	for idx := len(c.packetGroups) - 1; idx >= 0; idx-- {
		pg := c.packetGroups[idx]
		mst, dur, nbytes, fullness := pg.Traffic()
		if mst < threshold {
			break
		}

		if fullness < c.params.Config.RateMeasurementWindowFullnessMin {
			continue
		}

		totalDuration += dur
		totalBytes += nbytes
	}

	if totalDuration >= c.params.Config.RateMeasurementWindowDurationMin.Microseconds() {
		c.lock.Lock()
		c.estimatedAvailableChannelCapacity = int64(totalBytes) * 8 * 1e6 / totalDuration
		c.lock.Unlock()
	} else {
		c.params.Logger.Infow("not enough data to estimate available channel capacity", "totalDuration", totalDuration)
	}
}

func (c *congestionDetector) processFeedbackReport(fbr feedbackReport) {
	recvRefTime, isOutOfOrder := c.twccFeedback.ProcessReport(fbr.report, fbr.at)
	if isOutOfOrder {
		c.params.Logger.Infow("received out-of-order feedback report")
	}

	probingStartSequenceNumber, probingStartSequenceNumberOk := c.packetTracker.ProbingStartSequenceNumber()
	probingEndSequenceNumber, probingEndSequenceNumberOk := c.packetTracker.ProbingEndSequenceNumber()
	c.params.Logger.Infow(
		"probe sequence numbers",
		"startOk", probingStartSequenceNumberOk,
		"start", probingStartSequenceNumber,
		"endOk", probingEndSequenceNumberOk,
		"end", probingEndSequenceNumber,
	) // REMOVE

	if len(c.packetGroups) == 0 {
		c.packetGroups = append(
			c.packetGroups,
			newPacketGroup(
				packetGroupParams{
					Config: c.params.Config.PacketGroup,
					Logger: c.params.Logger,
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

		if c.probeGroup == nil && probingStartSequenceNumberOk && pi.sequenceNumber == probingStartSequenceNumber {
			// force finalize the active group and start the probe group
			pg.Finalize()

			c.probeGroup = newPacketGroup(
				packetGroupParams{
					Config: PacketGroupConfig{
						MinPackets:                 2000,
						MaxWindowDuration:          50000 * time.Millisecond,
						LossPenaltyMinPacketsRatio: 0.5,
						LossPenaltyFactor:          0.25,
					},
					Logger: c.params.Logger,
				},
				0,
			)
			pg = c.probeGroup
		}
		if c.probeGroup != nil {
			c.params.Logger.Infow("processing probe sequence", "packetInfo", pi, "sn", pi.sequenceNumber) // SSBWE-REMOVE
			pg = c.probeGroup
		}

		if c.probeGroup != nil && probingEndSequenceNumberOk && pi.sequenceNumber == probingEndSequenceNumber {
			c.probeGroup.Finalize()
			c.params.Logger.Infow("probe group done", "group", c.probeGroup) // SSBWE-REMOVE
			c.probeGroup = nil
		}

		err := pg.Add(pi, sendDelta, recvDelta, isLost)
		if err == nil {
			return
		}

		if err == errGroupFinalized {
			// previous group ended, start a new group
			c.updateTrend(pg.CapturedTrafficRatio())

			// SSBWE-REMOVE c.params.Logger.Infow("packet group done", "group", pg, "numGroups", len(c.packetGroups)) // SSBWE-REMOVE
			pqd, _ := pg.PropagatedQueuingDelay()
			pg = newPacketGroup(
				packetGroupParams{
					Config: c.params.Config.PacketGroup,
					Logger: c.params.Logger,
				},
				pqd,
			)
			c.packetGroups = append(c.packetGroups, pg)

			pg.Add(pi, sendDelta, recvDelta, isLost)
			return
		}

		// try an older group
		for idx := len(c.packetGroups) - 2; idx >= 0; idx-- {
			opg := c.packetGroups[idx]
			if err := opg.Add(pi, sendDelta, recvDelta, isLost); err == nil {
				return
			} else if err == errGroupFinalized {
				c.params.Logger.Infow("unpected finalized group", "packetInfo", pi, "packetGroup", opg)
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

			if c.GetCongestionState() == CongestionStateCongested {
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
