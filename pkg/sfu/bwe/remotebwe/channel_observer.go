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

package remotebwe

import (
	"fmt"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
)

// ------------------------------------------------

type channelTrend int

const (
	channelTrendNeutral channelTrend = iota
	channelTrendClearing
	channelTrendCongesting
)

func (c channelTrend) String() string {
	switch c {
	case channelTrendNeutral:
		return "NEUTRAL"
	case channelTrendClearing:
		return "CLEARING"
	case channelTrendCongesting:
		return "CONGESTING"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

// ------------------------------------------------
type channelCongestionReason int

const (
	channelCongestionReasonNone channelCongestionReason = iota
	channelCongestionReasonEstimate
	channelCongestionReasonLoss
)

func (c channelCongestionReason) String() string {
	switch c {
	case channelCongestionReasonNone:
		return "NONE"
	case channelCongestionReasonEstimate:
		return "ESTIMATE"
	case channelCongestionReasonLoss:
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

	defaultChannelObserverConfigProbe = ChannelObserverConfig{
		Estimate: defaultTrendDetectorConfigProbe,
		Nack:     defaultNackTrackerConfigProbe,
	}

	defaultTrendDetectorConfigNonProbe = ccutils.TrendDetectorConfig{
		RequiredSamples:        12,
		RequiredSamplesMin:     8,
		DownwardTrendThreshold: -0.6,
		DownwardTrendMaxWait:   5 * time.Second,
		CollapseThreshold:      500 * time.Millisecond,
		ValidityWindow:         10 * time.Second,
	}

	defaultChannelObserverConfigNonProbe = ChannelObserverConfig{
		Estimate: defaultTrendDetectorConfigNonProbe,
		Nack:     defaultNackTrackerConfigNonProbe,
	}
)

// ------------------------------------------------

type channelObserverParams struct {
	Name   string
	Config ChannelObserverConfig
}

type channelObserver struct {
	params channelObserverParams
	logger logger.Logger

	estimateTrend *ccutils.TrendDetector[int64]
	nackTracker   *nackTracker
}

func newChannelObserver(params channelObserverParams, logger logger.Logger) *channelObserver {
	return &channelObserver{
		params: params,
		logger: logger,
		estimateTrend: ccutils.NewTrendDetector[int64](ccutils.TrendDetectorParams{
			Name:   params.Name + "-estimate",
			Logger: logger,
			Config: params.Config.Estimate,
		}),
		nackTracker: newNackTracker(nackTrackerParams{
			Name:   params.Name + "-nack",
			Logger: logger,
			Config: params.Config.Nack,
		}),
	}
}

func (c *channelObserver) SeedEstimate(estimate int64) {
	c.estimateTrend.Seed(estimate)
}

func (c *channelObserver) AddEstimate(estimate int64) {
	c.estimateTrend.AddValue(estimate)
}

func (c *channelObserver) AddNack(packets uint32, repeatedNacks uint32) {
	c.nackTracker.Add(packets, repeatedNacks)
}

func (c *channelObserver) GetLowestEstimate() int64 {
	return c.estimateTrend.GetLowest()
}

func (c *channelObserver) GetHighestEstimate() int64 {
	return c.estimateTrend.GetHighest()
}

func (c *channelObserver) HasEnoughEstimateSamples() bool {
	return c.estimateTrend.HasEnoughSamples()
}

func (c *channelObserver) GetNackRatio() float64 {
	return c.nackTracker.GetRatio()
}

/* REMOTE-BWE-DATA
func (c *channelObserver) GetNackHistory() []string {
	return c.nackTracker.GetHistory()
}
*/

func (c *channelObserver) GetTrend() (channelTrend, channelCongestionReason) {
	estimateDirection := c.estimateTrend.GetDirection()

	switch {
	case estimateDirection == ccutils.TrendDirectionDownward:
		c.logger.Debugw("remote bwe: channel observer: estimate is trending downward", "channel", c)
		return channelTrendCongesting, channelCongestionReasonEstimate

	case c.nackTracker.IsTriggered():
		c.logger.Debugw("remote bwe: channel observer: high rate of repeated NACKs", "channel", c)
		return channelTrendCongesting, channelCongestionReasonLoss

	case estimateDirection == ccutils.TrendDirectionUpward:
		return channelTrendClearing, channelCongestionReasonNone
	}

	return channelTrendNeutral, channelCongestionReasonNone
}

func (c *channelObserver) String() string {
	return fmt.Sprintf("name: %s, estimate: {%v}, nack {%v}", c.params.Name, c.estimateTrend, c.nackTracker)
}

// ------------------------------------------------
