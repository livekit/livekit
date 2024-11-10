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
	CongestedEstimateTrend         ccutils.TrendDetectorConfig `yaml:"congested_estimate_trend,omitempty"`
	CongestedEstimateEpsilon       int64                       `yaml:"congested_estimate_epsilon,omitempty"`
}

var (
	defaultTrendDetectorConfigCongestedEstimate = ccutils.TrendDetectorConfig{
		RequiredSamples:        8,
		RequiredSamplesMin:     4,
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
		CongestedEstimateTrend:           defaultTrendDetectorConfigCongestedEstimate,
		CongestedEstimateEpsilon:         10000,
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
	congestedEstimateTrend            *ccutils.TrendDetector[int64]

	onCongestionStateChange func(congestionState CongestionState, estimatedAvailableChannelCapacity int64)
}

func NewCongestionDetector(params congestionDetectorParams) *congestionDetector {
	c := &congestionDetector{
		params:                            params,
		packetTracker:                     NewPacketTracker(packetTrackerParams{Logger: params.Logger}),
		twccFeedback:                      NewTWCCFeedback(twccFeedbackParams{Logger: params.Logger}),
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

	// when in congested state, monitor changes in estimate to ensure allocations
	// are in line with estimate, it is possible that the estimate is incorrect
	// congestion starts and the allocation mayb be sub-optimal and not enough to
	// relieve congestion, by monitoing on a continuous basis allocations can be
	// adjusted in the direction of releving congestion
	if state == CongestionStateCongested && prevState != CongestionStateCongested {
		c.congestedEstimateTrend = ccutils.NewTrendDetector[int64](ccutils.TrendDetectorParams{
			Name:   "ssbwe-estimate",
			Logger: c.params.Logger,
			Config: c.params.Config.CongestedEstimateTrend,
		})
	} else if state != CongestionStateCongested {
		c.congestedEstimateTrend = nil
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

func (c *congestionDetector) updateTrend(estimatedAvailableChannelCapacity int64) {
	if c.congestedEstimateTrend == nil {
		return
	}

	c.congestedEstimateTrend.AddValue((estimatedAvailableChannelCapacity + c.params.Config.CongestedEstimateEpsilon - 1) / c.params.Config.CongestedEstimateEpsilon * c.params.Config.CongestedEstimateEpsilon)

	if c.congestedEstimateTrend.GetDirection() == ccutils.TrendDirectionDownward {
		c.params.Logger.Infow("estimate is trending downward", "channel", c.congestedEstimateTrend.ToString())
		estimatedAvailableChannelCapacity := c.congestedEstimateTrend.GetLowest()

		c.lock.RLock()
		state := c.congestionState
		onCongestionStateChange := c.onCongestionStateChange
		c.lock.RUnlock()

		if onCongestionStateChange != nil {
			onCongestionStateChange(state, estimatedAvailableChannelCapacity)
		}

		// reset to get new set of samples for next trend
		c.congestedEstimateTrend = ccutils.NewTrendDetector[int64](ccutils.TrendDetectorParams{
			Name:   "ssbwe-estimate",
			Logger: c.params.Logger,
			Config: c.params.Config.CongestedEstimateTrend,
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
		estimatedAvailableChannelCapacity := int64(totalBytes) * 8 * 1e6 / totalDuration

		c.lock.Lock()
		c.estimatedAvailableChannelCapacity = estimatedAvailableChannelCapacity
		c.lock.Unlock()

		c.updateTrend(estimatedAvailableChannelCapacity)
	} else {
		c.params.Logger.Infow("not enough data to estimate available channel capacity", "totalDuration", totalDuration)
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
			NewPacketGroup(
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

		err := pg.Add(pi, sendDelta, recvDelta, isLost)
		if err == nil {
			return
		}

		if err == errGroupFinalized {
			// previous group ended, start a new group
			c.params.Logger.Infow("packet group done", "group", pg, "numGroups", len(c.packetGroups)) // SSBWE-REMOVE
			pqd, _ := pg.PropagatedQueuingDelay()
			pg = NewPacketGroup(
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
