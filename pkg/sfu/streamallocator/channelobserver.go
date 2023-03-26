package streamallocator

import (
	"fmt"
	"time"

	"github.com/livekit/protocol/logger"
)

// ------------------------------------------------

type ChannelTrend int

const (
	ChannelTrendNeutral ChannelTrend = iota
	ChannelTrendClearing
	ChannelTrendCongesting
)

func (c ChannelTrend) String() string {
	switch c {
	case ChannelTrendNeutral:
		return "NEUTRAL"
	case ChannelTrendClearing:
		return "CLEARING"
	case ChannelTrendCongesting:
		return "CONGESTING"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

// ------------------------------------------------

type ChannelCongestionReason int

const (
	ChannelCongestionReasonNone ChannelCongestionReason = iota
	ChannelCongestionReasonEstimate
	ChannelCongestionReasonLoss
)

func (c ChannelCongestionReason) String() string {
	switch c {
	case ChannelCongestionReasonNone:
		return "NONE"
	case ChannelCongestionReasonEstimate:
		return "ESTIMATE"
	case ChannelCongestionReasonLoss:
		return "LOSS"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

// ------------------------------------------------

type ChannelObserverParams struct {
	Name                           string
	EstimateRequiredSamples        int
	EstimateDownwardTrendThreshold float64
	EstimateCollapseValues         bool
	NackWindowMinDuration          time.Duration
	NackWindowMaxDuration          time.Duration
	NackRatioThreshold             float64
}

type ChannelObserver struct {
	params ChannelObserverParams
	logger logger.Logger

	estimateTrend *TrendDetector

	nackWindowStartTime time.Time
	packets             uint32
	repeatedNacks       uint32
}

func NewChannelObserver(params ChannelObserverParams, logger logger.Logger) *ChannelObserver {
	return &ChannelObserver{
		params: params,
		logger: logger,
		estimateTrend: NewTrendDetector(TrendDetectorParams{
			Name:                   params.Name + "-estimate",
			Logger:                 logger,
			RequiredSamples:        params.EstimateRequiredSamples,
			DownwardTrendThreshold: params.EstimateDownwardTrendThreshold,
			CollapseValues:         params.EstimateCollapseValues,
		}),
	}
}

func (c *ChannelObserver) SeedEstimate(estimate int64) {
	c.estimateTrend.Seed(estimate)
}

func (c *ChannelObserver) SeedNack(packets uint32, repeatedNacks uint32) {
	c.packets = packets
	c.repeatedNacks = repeatedNacks
}

func (c *ChannelObserver) AddEstimate(estimate int64) {
	c.estimateTrend.AddValue(estimate)
}

func (c *ChannelObserver) AddNack(packets uint32, repeatedNacks uint32) {
	if c.params.NackWindowMaxDuration != 0 && !c.nackWindowStartTime.IsZero() && time.Since(c.nackWindowStartTime) > c.params.NackWindowMaxDuration {
		c.nackWindowStartTime = time.Time{}
		c.packets = 0
		c.repeatedNacks = 0
	}

	//
	// Start NACK monitoring window only when a repeated NACK happens.
	// This allows locking tightly to when NACKs start happening and
	// check if the NACKs keep adding up (potentially a sign of congestion)
	// or isolated losses
	//
	if c.repeatedNacks == 0 && repeatedNacks != 0 {
		c.nackWindowStartTime = time.Now()
	}

	if !c.nackWindowStartTime.IsZero() {
		c.packets += packets
		c.repeatedNacks += repeatedNacks
	}
}

func (c *ChannelObserver) GetLowestEstimate() int64 {
	return c.estimateTrend.GetLowest()
}

func (c *ChannelObserver) GetHighestEstimate() int64 {
	return c.estimateTrend.GetHighest()
}

func (c *ChannelObserver) GetNackRatio() (uint32, uint32, float64) {
	ratio := 0.0
	if c.packets != 0 {
		ratio = float64(c.repeatedNacks) / float64(c.packets)
		if ratio > 1.0 {
			ratio = 1.0
		}
	}

	return c.packets, c.repeatedNacks, ratio
}

func (c *ChannelObserver) GetTrend() (ChannelTrend, ChannelCongestionReason) {
	estimateDirection := c.estimateTrend.GetDirection()
	packets, repeatedNacks, nackRatio := c.GetNackRatio()

	switch {
	case estimateDirection == TrendDirectionDownward:
		c.logger.Debugw(
			"stream allocator: channel observer: estimate is trending downward",
			"name", c.params.Name,
			"estimate", c.estimateTrend.ToString(),
			"packets", packets,
			"repeatedNacks", repeatedNacks,
			"ratio", nackRatio,
		)
		return ChannelTrendCongesting, ChannelCongestionReasonEstimate
	case c.params.NackWindowMinDuration != 0 && !c.nackWindowStartTime.IsZero() && time.Since(c.nackWindowStartTime) > c.params.NackWindowMinDuration && nackRatio > c.params.NackRatioThreshold:
		c.logger.Debugw(
			"stream allocator: channel observer: high rate of repeated NACKs",
			"name", c.params.Name,
			"estimate", c.estimateTrend.ToString(),
			"packets", packets,
			"repeatedNacks", repeatedNacks,
			"ratio", nackRatio,
		)
		return ChannelTrendCongesting, ChannelCongestionReasonLoss
	case estimateDirection == TrendDirectionUpward:
		return ChannelTrendClearing, ChannelCongestionReasonNone
	}

	return ChannelTrendNeutral, ChannelCongestionReasonNone
}

// ------------------------------------------------
