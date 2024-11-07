package sendsidebwe

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/gammazero/deque"
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
		MinNumberOfGroups: 2,
		MinDuration:       100 * time.Millisecond,
	}

	DefaultCongestedCongestionSignalConfig = CongestionSignalConfig{
		MinNumberOfGroups: 4,
		MinDuration:       300 * time.Millisecond,
	}
)

// -------------------------------------------------------------------------------

type CongestionDetectorConfig struct {
	PacketGroup                      PacketGroupConfig      `yaml:"packet_group,omitempty"`
	PacketGroupMaxAge                time.Duration          `yaml:"packet_group_max_age,omitempty"`
	JQRMinDelay                      time.Duration          `yaml:"jqr_min_delay,omitempty"`
	DQRMaxDelay                      time.Duration          `yaml:"dqr_max_delay,omitempty"`
	EarlyWarning                     CongestionSignalConfig `yaml:"early_warning,omitempty"`
	EarlyWarningHangover             time.Duration          `yaml:"early_warning_hangover,omitempty"`
	Congested                        CongestionSignalConfig `yaml:"congested,omitempty"`
	CongestedHangover                time.Duration          `yaml:"congested_hangover,omitempty"`
	RateMeasurementWindowDurationMin time.Duration          `yaml:"rate_measurement_window_duration_min,omitempty"`
	RateMeasurementWindowDurationMax time.Duration          `yaml:"rate_measurement_window_duration_max,omitempty"`
}

var (
	DefaultCongestionDetectorConfig = CongestionDetectorConfig{
		PacketGroup:                      DefaultPacketGroupConfig,
		PacketGroupMaxAge:                30 * time.Second,
		JQRMinDelay:                      15 * time.Millisecond,
		DQRMaxDelay:                      5 * time.Millisecond,
		EarlyWarning:                     DefaultEarlyWarningCongestionSignalConfig,
		EarlyWarningHangover:             500 * time.Millisecond,
		Congested:                        DefaultCongestedCongestionSignalConfig,
		CongestedHangover:                3 * time.Second,
		RateMeasurementWindowDurationMin: 800 * time.Millisecond,
		RateMeasurementWindowDurationMax: 2 * time.Second,
	}
)

// -------------------------------------------------------------------------------

type feedbackReport struct {
	at     time.Time
	report *rtcp.TransportLayerCC
}

type CongestionDetectorParams struct {
	Config CongestionDetectorConfig
	Logger logger.Logger
}

type CongestionDetector struct {
	params CongestionDetectorParams

	lock            sync.RWMutex
	feedbackReports deque.Deque[feedbackReport]

	*PacketTracker
	twccFeedback *TWCCFeedback

	packetGroups      []*PacketGroup
	activePacketGroup *PacketGroup

	wake chan struct{}
	stop core.Fuse

	estimatedAvailableChannelCapacity int64
	congestionState                   CongestionState
	congestionStateSwitchedAt         time.Time
	onCongestionStateChange           func(congestionState CongestionState, estimatedAvailableChannelCapacity int64)

	debugFile *os.File
}

func NewCongestionDetector(params CongestionDetectorParams) *CongestionDetector {
	c := &CongestionDetector{
		params:                            params,
		PacketTracker:                     NewPacketTracker(PacketTrackerParams{Logger: params.Logger}),
		twccFeedback:                      NewTWCCFeedback(TWCCFeedbackParams{Logger: params.Logger}),
		wake:                              make(chan struct{}, 1),
		estimatedAvailableChannelCapacity: 100_000_000,
	}

	c.feedbackReports.SetMinCapacity(3)

	c.debugFile, _ = os.CreateTemp("/tmp", "twcc")

	go c.worker()
	return c
}

func (c *CongestionDetector) Stop() {
	c.stop.Once(func() {
		close(c.wake)
		if c.debugFile != nil {
			c.debugFile.Close()
		}
	})
}

func (c *CongestionDetector) OnCongestionStateChange(f func(congestionState CongestionState, estimatedAvailableChannelCapacity int64)) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.onCongestionStateChange = f
}

func (c *CongestionDetector) GetCongestionState() CongestionState {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.congestionState
}

func (c *CongestionDetector) updateCongestionState(state CongestionState) {
	c.lock.Lock()
	c.params.Logger.Infow(
		"congestion state change",
		"from", c.congestionState,
		"to", state,
		"estimatedAvailableChannelCapacity", c.estimatedAvailableChannelCapacity,
	)
	c.congestionState = state

	onCongestionStateChange := c.onCongestionStateChange
	estimatedAvailableChannelCapacity := c.estimatedAvailableChannelCapacity
	c.lock.Unlock()

	if onCongestionStateChange != nil {
		onCongestionStateChange(state, estimatedAvailableChannelCapacity)
	}
}

func (c *CongestionDetector) GetEstimatedAvailableChannelCapacity() int64 {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.estimatedAvailableChannelCapacity
}

func (c *CongestionDetector) HandleRTCP(report *rtcp.TransportLayerCC) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.feedbackReports.PushBack(feedbackReport{mono.Now(), report})

	// notify worker of a new feedback
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *CongestionDetector) prunePacketGroups() {
	if c.activePacketGroup == nil {
		return
	}

	activeMinSendTime, ok := c.activePacketGroup.MinSendTime()
	if !ok {
		return
	}

	threshold := activeMinSendTime - c.params.Config.PacketGroupMaxAge.Microseconds()
	for idx, pg := range c.packetGroups {
		if mst, ok := pg.MinSendTime(); ok {
			if mst < threshold {
				c.packetGroups = c.packetGroups[idx+1:]
				return
			}
		}
	}
}

func (c *CongestionDetector) isCongestionSignalTriggered() (bool, bool) {
	earlyWarningTriggered := false
	congestedTriggered := false

	numGroups := 0
	duration := int64(0)
	for idx := len(c.packetGroups) - 1; idx >= 0; idx-- {
		pg := c.packetGroups[idx]
		pqd := pg.PropagatedQueuingDelay()
		if pqd > c.params.Config.JQRMinDelay.Microseconds() {
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

func (c *CongestionDetector) congestionDetectionStateMachine() {
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
			newState = CongestionStateEarlyWarningRelieving
		}

	case CongestionStateEarlyWarningRelieving:
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
			newState = CongestionStateCongestedRelieving
		}

	case CongestionStateCongestedRelieving:
		if time.Since(c.congestionStateSwitchedAt) >= c.params.Config.CongestedHangover {
			if congestedTriggered {
				c.params.Logger.Warnw("invalid congested state transition", nil, "from", state)
			}
			if earlyWarningTriggered {
				newState = CongestionStateEarlyWarning
			} else {
				newState = CongestionStateNone
			}
		}
	}

	c.estimateAvailableChannelCapacity()

	if newState != state {
		c.congestionStateSwitchedAt = mono.Now()
		c.updateCongestionState(newState)
	}
}

func (c *CongestionDetector) estimateAvailableChannelCapacity() {
	if c.activePacketGroup == nil {
		return
	}

	activeMinSendTime, ok := c.activePacketGroup.MinSendTime()
	if !ok {
		return
	}

	totalDuration := int64(0)
	totalBytes := float64(0.0)
	threshold := activeMinSendTime - c.params.Config.RateMeasurementWindowDurationMax.Microseconds()
	for _, pg := range c.packetGroups {
		if mst, dur, nbytes, ctr, ok := pg.Traffic(); ok {
			if mst < threshold {
				break
			}

			totalDuration += dur
			// captured traffic ratio is a measure of what fraction of sent traffic was delivered
			totalBytes += float64(dur) * float64(nbytes) * ctr / 1e6
		}
	}

	if totalDuration >= c.params.Config.RateMeasurementWindowDurationMin.Microseconds() {
		c.lock.Lock()
		c.estimatedAvailableChannelCapacity = int64(totalBytes * 1e6 / float64(totalDuration))
		c.lock.Unlock()
	} else {
		c.params.Logger.Infow("not enough data to estimate available channel capacity", "totalDuration", totalDuration)
	}
}

func (c *CongestionDetector) processFeedbackReport(fbr feedbackReport) {
	report, err := c.twccFeedback.GetReport(fbr.report, fbr.at)
	if err != nil {
		return
	}

	now := mono.UnixMicro()
	if c.debugFile != nil {
		toWrite := fmt.Sprintf("REPORT: start: %d", now)
		c.debugFile.WriteString(toWrite)
		c.debugFile.WriteString("\n")
	}

	if c.activePacketGroup == nil {
		c.activePacketGroup = NewPacketGroup(
			PacketGroupParams{
				Config: c.params.Config.PacketGroup,
				Logger: c.params.Logger,
			},
			0,
		)
	}

	trackPacketGroup := func(pi *packetInfo, piPrev *packetInfo) {
		if pi != nil && piPrev != nil {
			if err := c.activePacketGroup.Add(pi, piPrev); err != nil {
				c.packetGroups = append(c.packetGroups, c.activePacketGroup)
				c.params.Logger.Infow("packet group done", "group", c.activePacketGroup) // REMOVE

				c.activePacketGroup = NewPacketGroup(
					PacketGroupParams{
						Config: c.params.Config.PacketGroup,
						Logger: c.params.Logger,
					},
					c.activePacketGroup.PropagatedQueuingDelay(),
				)
				c.activePacketGroup.Add(pi, piPrev)
			}
		}

		if c.debugFile != nil && pi != nil {
			toInt := func(a bool) int {
				if a {
					return 1
				}

				return 0
			}

			toWrite := fmt.Sprintf(
				"PACKET: sn: %d, headerSize: %d, payloadSize: %d, isRTX: %d, sendTime: %d, recvTime: %d",
				pi.sn,
				pi.headerSize,
				pi.payloadSize,
				toInt(pi.isRTX),
				pi.sendTime,
				pi.recvTime,
			)
			c.debugFile.WriteString(toWrite)
			c.debugFile.WriteString("\n")
		}
	}

	// 1. go through the TWCC feedback report and record recive time as reported by remote
	// 2. process acknowledged packet and group them
	sn := report.BaseSequenceNumber
	deltaIdx := 0
	recvRefTime := int64(report.ReferenceTime) * 64 * 1000 // in us
	for _, chunk := range report.PacketChunks {
		switch chunk := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for i := uint16(0); i < chunk.RunLength; i++ {
				if chunk.PacketStatusSymbol != rtcp.TypeTCCPacketNotReceived {
					recvRefTime += report.RecvDeltas[deltaIdx].Delta
					deltaIdx++

					pi, piPrev := c.PacketTracker.RecordPacketReceivedByRemote(sn, recvRefTime)
					trackPacketGroup(pi, piPrev)
				}
				sn++
			}

		case *rtcp.StatusVectorChunk:
			for _, symbol := range chunk.SymbolList {
				if symbol != rtcp.TypeTCCPacketNotReceived {
					recvRefTime += report.RecvDeltas[deltaIdx].Delta
					deltaIdx++

					pi, piPrev := c.PacketTracker.RecordPacketReceivedByRemote(sn, recvRefTime)
					trackPacketGroup(pi, piPrev)
				}
				sn++
			}
		}
	}

	if c.debugFile != nil {
		toWrite := fmt.Sprintf("REPORT: end: %d", now)
		c.debugFile.WriteString(toWrite)
		c.debugFile.WriteString("\n")
	}

	c.prunePacketGroups()
	c.congestionDetectionStateMachine()
}

func (c *CongestionDetector) worker() {
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

		case <-c.stop.Watch():
			return
		}
	}
}
