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

// SSBWE-TODO: CongestionDetectorConfig
//   - how much of packet group history to keep
//   - settings to declare JQR
//     o threshold of queuing delay + group delay
//   - settings to declare DQR
//     o threshold of queuing delay + group delay
//   - settings to declare congestion onset
//     o min number of groups
//     o min time
//   - settings to declare congestion
//     o min number of groups
//     o min time
//   - settings to declare congestion relieved
//     o min time of continuous DQR (and maybe INDETERMINATE)
type CongestionDetectorConfig struct {
	PacketGroupMaxAge time.Duration `yaml:"packet_group_max_age,omitempty"`
}

var (
	DefaultCongestionDetectorConfig = CongestionDetectorConfig{
		PacketGroupMaxAge: 30 * time.Second,
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

	packetGroups      []*PacketGroup // SSBWE-TODO - prune packet groups to some recent history
	activePacketGroup *PacketGroup

	wake chan struct{}
	stop core.Fuse

	// SSBWE-TODO: these are not implemented yet
	estimatedChannelCapacity int64
	congestionState          CongestionState
	onCongestionStateChange  func(congestionState CongestionState, channelCapacity int64)

	debugFile *os.File
}

func NewCongestionDetector(params CongestionDetectorParams) *CongestionDetector {
	c := &CongestionDetector{
		params:                   params,
		PacketTracker:            NewPacketTracker(PacketTrackerParams{Logger: params.Logger}),
		twccFeedback:             NewTWCCFeedback(TWCCFeedbackParams{Logger: params.Logger}),
		wake:                     make(chan struct{}, 1),
		estimatedChannelCapacity: 100_000_000,
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

func (c *CongestionDetector) OnCongestionStateChange(f func(congestionState CongestionState, channelCapacity int64)) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.onCongestionStateChange = f
}

func (c *CongestionDetector) GetCongestionState() CongestionState {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.congestionState
}

func (c *CongestionDetector) GetEstimatedChannelCapacity() int64 {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.estimatedChannelCapacity
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
				// SSBWE-TODO: should take config from params
				Config: DefaultPacketGroupConfig,
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
						// SSBWE-TODO: should take config from params
						Config: DefaultPacketGroupConfig,
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

	// SSBWE-TODO:
	//  - run congestion detection
	//  - run bandwidth estimation
	//  - do callbacks as necessary
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
