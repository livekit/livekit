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

package streamallocator

import (
	"fmt"

	"github.com/livekit/livekit-server/pkg/config"
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
	Name   string
	Config config.CongestionControlChannelObserverConfig
}

type ChannelObserver struct {
	params ChannelObserverParams
	logger logger.Logger

	estimateTrend *TrendDetector
	nackTracker   *NackTracker
}

func NewChannelObserver(params ChannelObserverParams, logger logger.Logger) *ChannelObserver {
	return &ChannelObserver{
		params: params,
		logger: logger,
		estimateTrend: NewTrendDetector(TrendDetectorParams{
			Name:                   params.Name + "-estimate",
			Logger:                 logger,
			RequiredSamples:        params.Config.EstimateRequiredSamples,
			RequiredSamplesMin:     params.Config.EstimateRequiredSamplesMin,
			DownwardTrendThreshold: params.Config.EstimateDownwardTrendThreshold,
			DownwardTrendMaxWait:   params.Config.EstimateDownwardTrendMaxWait,
			CollapseThreshold:      params.Config.EstimateCollapseThreshold,
			ValidityWindow:         params.Config.EstimateValidityWindow,
		}),
		nackTracker: NewNackTracker(NackTrackerParams{
			Name:              params.Name + "-nack",
			Logger:            logger,
			WindowMinDuration: params.Config.NackWindowMinDuration,
			WindowMaxDuration: params.Config.NackWindowMaxDuration,
			RatioThreshold:    params.Config.NackRatioThreshold,
		}),
	}
}

func (c *ChannelObserver) SeedEstimate(estimate int64) {
	c.estimateTrend.Seed(estimate)
}

func (c *ChannelObserver) AddEstimate(estimate int64) {
	c.estimateTrend.AddValue(estimate)
}

func (c *ChannelObserver) AddNack(packets uint32, repeatedNacks uint32) {
	c.nackTracker.Add(packets, repeatedNacks)
}

func (c *ChannelObserver) GetLowestEstimate() int64 {
	return c.estimateTrend.GetLowest()
}

func (c *ChannelObserver) GetHighestEstimate() int64 {
	return c.estimateTrend.GetHighest()
}

func (c *ChannelObserver) HasEnoughEstimateSamples() bool {
	return c.estimateTrend.HasEnoughSamples()
}

func (c *ChannelObserver) GetNackRatio() float64 {
	return c.nackTracker.GetRatio()
}

/* STREAM-ALLOCATOR-DATA
func (c *ChannelObserver) GetNackHistory() []string {
	return c.nackTracker.GetHistory()
}
*/

func (c *ChannelObserver) GetTrend() (ChannelTrend, ChannelCongestionReason) {
	estimateDirection := c.estimateTrend.GetDirection()

	switch {
	case estimateDirection == TrendDirectionDownward:
		c.logger.Debugw("stream allocator: channel observer: estimate is trending downward", "channel", c.ToString())
		return ChannelTrendCongesting, ChannelCongestionReasonEstimate

	case c.nackTracker.IsTriggered():
		c.logger.Debugw("stream allocator: channel observer: high rate of repeated NACKs", "channel", c.ToString())
		return ChannelTrendCongesting, ChannelCongestionReasonLoss

	case estimateDirection == TrendDirectionUpward:
		return ChannelTrendClearing, ChannelCongestionReasonNone
	}

	return ChannelTrendNeutral, ChannelCongestionReasonNone
}

func (c *ChannelObserver) ToString() string {
	return fmt.Sprintf("name: %s, estimate: {%s}, nack {%s}", c.params.Name, c.estimateTrend.ToString(), c.nackTracker.ToString())
}

// ------------------------------------------------
