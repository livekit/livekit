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
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
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

type ChannelObserverConfig struct {
	Estimate ccutils.TrendDetectorConfig `yaml:"estimate,omitempty"`
	Nack     NackTrackerConfig           `yaml:"nack,omitempty"`
}

var (
	defaultTrendDetectorConfigProbe = ccutils.TrendDetectorConfig{
		RequiredSamples:        3,
		RequiredSamplesMin:     3,
		DownwardTrendThreshold: 0.0,
		DownwardTrendMaxWait:   5 * time.Second,
		CollapseThreshold:      0,
		ValidityWindow:         10 * time.Second,
	}

	DefaultChannelObserverConfigProbe = ChannelObserverConfig{
		Estimate: defaultTrendDetectorConfigProbe,
		Nack:     DefaultNackTrackerConfigProbe,
	}

	defaultTrendDetectorConfigNonProbe = ccutils.TrendDetectorConfig{
		RequiredSamples:        12,
		RequiredSamplesMin:     8,
		DownwardTrendThreshold: -0.6,
		DownwardTrendMaxWait:   5 * time.Second,
		CollapseThreshold:      500 * time.Millisecond,
		ValidityWindow:         10 * time.Second,
	}

	DefaultChannelObserverConfigNonProbe = ChannelObserverConfig{
		Estimate: defaultTrendDetectorConfigNonProbe,
		Nack:     DefaultNackTrackerConfigNonProbe,
	}
)

// ------------------------------------------------

type ChannelObserverParams struct {
	Name   string
	Config ChannelObserverConfig
}

type ChannelObserver struct {
	params ChannelObserverParams
	logger logger.Logger

	estimateTrend *ccutils.TrendDetector
	nackTracker   *NackTracker
}

func NewChannelObserver(params ChannelObserverParams, logger logger.Logger) *ChannelObserver {
	return &ChannelObserver{
		params: params,
		logger: logger,
		estimateTrend: ccutils.NewTrendDetector(ccutils.TrendDetectorParams{
			Name:   params.Name + "-estimate",
			Logger: logger,
			Config: params.Config.Estimate,
		}),
		nackTracker: NewNackTracker(NackTrackerParams{
			Name:   params.Name + "-nack",
			Logger: logger,
			Config: params.Config.Nack,
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
	case estimateDirection == ccutils.TrendDirectionDownward:
		c.logger.Debugw("stream allocator: channel observer: estimate is trending downward", "channel", c.ToString())
		return ChannelTrendCongesting, ChannelCongestionReasonEstimate

	case c.nackTracker.IsTriggered():
		c.logger.Debugw("stream allocator: channel observer: high rate of repeated NACKs", "channel", c.ToString())
		return ChannelTrendCongesting, ChannelCongestionReasonLoss

	case estimateDirection == ccutils.TrendDirectionUpward:
		return ChannelTrendClearing, ChannelCongestionReasonNone
	}

	return ChannelTrendNeutral, ChannelCongestionReasonNone
}

func (c *ChannelObserver) ToString() string {
	return fmt.Sprintf("name: %s, estimate: {%s}, nack {%s}", c.params.Name, c.estimateTrend.ToString(), c.nackTracker.ToString())
}

// ------------------------------------------------
