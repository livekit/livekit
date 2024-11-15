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
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

const (
	ChannelCapacityInfinity = 100 * 1000 * 1000 // 100 Mbps
)

// ---------------------------------------------------------------------------

type RemoteBWEConfig struct {
	NackRatioAttenuator     float64               `yaml:"nack_ratio_attenuator,omitempty"`
	ExpectedUsageThreshold  float64               `yaml:"expected_usage_threshold,omitempty"`
	ChannelObserverProbe    ChannelObserverConfig `yaml:"channel_observer_probe,omitempty"`
	ChannelObserverNonProbe ChannelObserverConfig `yaml:"channel_observer_non_probe,omitempty"`
	CongestedMinDuration    time.Duration         `yaml:"congested_min_duration,omitempty"`

	PeriodicCheckInterval          time.Duration `yaml:"periodic_check_interval,omitempty"`
	PeriodicCheckIntervalCongested time.Duration `yaml:"periodic_check_interval_congested,omitempty"`
}

var (
	DefaultRemoteBWEConfig = RemoteBWEConfig{
		NackRatioAttenuator:            0.4,
		ExpectedUsageThreshold:         0.95,
		ChannelObserverProbe:           defaultChannelObserverConfigProbe,
		ChannelObserverNonProbe:        defaultChannelObserverConfigNonProbe,
		CongestedMinDuration:           3 * time.Second,
		PeriodicCheckInterval:          2 * time.Second,
		PeriodicCheckIntervalCongested: 200 * time.Millisecond,
	}
)

// ---------------------------------------------------------------------------

type RemoteBWEParams struct {
	Config RemoteBWEConfig
	Logger logger.Logger
}

type RemoteBWE struct {
	bwe.NullBWE

	params RemoteBWEParams

	lock sync.RWMutex

	lastReceivedEstimate       int64
	lastExpectedBandwidthUsage int64
	isInProbe                  bool
	committedChannelCapacity   int64

	channelObserver *channelObserver

	congestionState           bwe.CongestionState
	congestionStateSwitchedAt time.Time

	wake chan struct{}
	stop core.Fuse

	bweListener bwe.BWEListener
}

func NewRemoteBWE(params RemoteBWEParams) *RemoteBWE {
	r := &RemoteBWE{
		params: params,
	}
	r.channelObserver = r.newChannelObserverNonProbe()
	return r
}

func (r *RemoteBWE) SetBWEListener(bweListener bwe.BWEListener) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.bweListener = bweListener
}

func (r *RemoteBWE) getBWEListener() bwe.BWEListener {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.bweListener
}

func (r *RemoteBWE) Reset() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.channelObserver = r.newChannelObserverNonProbe()
}

func (r *RemoteBWE) Stop() {
	r.stop.Break()
}

func (r *RemoteBWE) HandleREMB(
	receivedEstimate int64,
	isProbeFinalizing bool,
	expectedBandwidthUsage int64,
	sentPackets uint32,
	repeatedNacks uint32,
) {
	r.lock.Lock()
	r.lastReceivedEstimate = receivedEstimate
	r.lastExpectedBandwidthUsage = expectedBandwidthUsage

	if !isProbeFinalizing {
		r.channelObserver.AddEstimate(r.lastReceivedEstimate)
		r.channelObserver.AddNack(sentPackets, repeatedNacks)
	}

	var (
		shouldNotify             bool
		state                    bwe.CongestionState
		committedChannelCapacity int64
	)
	if !r.isInProbe {
		shouldNotify, state, committedChannelCapacity = r.congestionDetectionStateMachine()
	}
	r.lock.Unlock()

	if shouldNotify {
		if bweListener := r.getBWEListener(); bweListener != nil {
			bweListener.OnCongestionStateChange(state, committedChannelCapacity)
		}
	}
}

func (r *RemoteBWE) congestionDetectionStateMachine() (bool, bwe.CongestionState, int64) {
	newState := r.congestionState
	update := false
	trend, reason := r.channelObserver.GetTrend()
	switch r.congestionState {
	case bwe.CongestionStateNone:
		if trend == bwe.ChannelTrendCongesting {
			if r.estimateAvailableChannelCapacity(reason) {
				newState = bwe.CongestionStateCongested
			}
		}

	case bwe.CongestionStateCongested:
		if trend == bwe.ChannelTrendCongesting {
			if r.estimateAvailableChannelCapacity(reason) {
				// update state sa this needs to reset switch time to wait for congestion min duration again
				update = true
			}
		} else if time.Since(r.congestionStateSwitchedAt) >= r.params.Config.CongestedMinDuration {
			newState = bwe.CongestionStateNone
		}
	}

	shouldNotify := false
	if newState != r.congestionState || update {
		r.updateCongestionState(newState, reason)
		shouldNotify = true
	}

	return shouldNotify, r.congestionState, r.committedChannelCapacity
}

func (r *RemoteBWE) estimateAvailableChannelCapacity(reason channelCongestionReason) bool {
	var estimateToCommit int64
	switch reason {
	case channelCongestionReasonLoss:
		estimateToCommit = int64(float64(r.lastExpectedBandwidthUsage) * (1.0 - r.params.Config.NackRatioAttenuator*r.channelObserver.GetNackRatio()))
	default:
		estimateToCommit = r.lastReceivedEstimate
	}
	if estimateToCommit > r.lastReceivedEstimate {
		estimateToCommit = r.lastReceivedEstimate
	}

	commitThreshold := int64(r.params.Config.ExpectedUsageThreshold * float64(r.lastExpectedBandwidthUsage))
	action := "applying"
	if estimateToCommit > commitThreshold {
		action = "skipping"
	}

	if action == "applying" {
		r.params.Logger.Infow(
			fmt.Sprintf("remote bwe: channel congestion detected, %s channel capacity update", action),
			"reason", reason,
			"old(bps)", r.committedChannelCapacity,
			"new(bps)", estimateToCommit,
			"lastReceived(bps)", r.lastReceivedEstimate,
			"expectedUsage(bps)", r.lastExpectedBandwidthUsage,
			"commitThreshold(bps)", commitThreshold,
			"channel", r.channelObserver.ToString(),
		)
	} else {
		r.params.Logger.Debugw(
			fmt.Sprintf("remote bwe: channel congestion detected, %s channel capacity update", action),
			"reason", reason,
			"old(bps)", r.committedChannelCapacity,
			"new(bps)", estimateToCommit,
			"lastReceived(bps)", r.lastReceivedEstimate,
			"expectedUsage(bps)", r.lastExpectedBandwidthUsage,
			"commitThreshold(bps)", commitThreshold,
			"channel", r.channelObserver.ToString(),
		)
	}
	/*
		r.params.Logger.Debugw(
			fmt.Sprintf("remote bwe: channel congestion detected, %s channel capacity: experimental", action),
			"nackHistory", r.channelObserver.GetNackHistory(),
		)
	*/
	if estimateToCommit > commitThreshold {
		return false
	}

	r.committedChannelCapacity = estimateToCommit

	// reset to get new set of samples for next trend
	r.channelObserver = r.newChannelObserverNonProbe()
	return true
}

func (r *RemoteBWE) updateCongestionState(state bwe.CongestionState, reason channelCongestionReason) {
	r.params.Logger.Infow(
		"remote bwe: congestion state change",
		"from", r.congestionState,
		"to", state,
		"reason", reason,
		"committedChannelCapacity", r.committedChannelCapacity,
	)

	if state != r.congestionState {
		// notify worker for ticker interval management based on state
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}

	r.congestionState = state
	r.congestionStateSwitchedAt = mono.Now()
}

func (r *RemoteBWE) newChannelObserverNonProbe() *channelObserver {
	return newChannelObserver(
		channelObserverParams{
			Name:   "non-probe",
			Config: r.params.Config.ChannelObserverNonProbe,
		},
		r.params.Logger,
	)
}

func (r *RemoteBWE) ProbingStart(expectedBandwidthUsage int64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.isInProbe = true
	r.lastExpectedBandwidthUsage = expectedBandwidthUsage

	channelState := ""
	if r.channelObserver != nil {
		channelState = r.channelObserver.ToString()
	}

	r.params.Logger.Debugw(
		"stream allocator: starting probe",
		"lastReceived", r.lastReceivedEstimate,
		"expectedBandwidthUsage", expectedBandwidthUsage,
		"channel", channelState,
	)

	r.channelObserver = newChannelObserver(
		channelObserverParams{
			Name:   "probe",
			Config: r.params.Config.ChannelObserverProbe,
		},
		r.params.Logger,
	)
	r.channelObserver.SeedEstimate(r.lastReceivedEstimate)
}

func (r *RemoteBWE) ProbingEnd(isNotFailing bool, isGoalReached bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	highestEstimateInProbe := r.channelObserver.GetHighestEstimate()

	//
	// Reset estimator at the end of a probe irrespective of probe result to get fresh readings.
	// With a failed probe, the latest estimate could be lower than committed estimate.
	// As bandwidth estimator (remote in REMB case, local in TWCC case) holds state,
	// subsequent estimates could start from the lower point. That should not trigger a
	// downward trend and get latched to committed estimate as that would trigger a re-allocation.
	// With fresh readings, as long as the trend is not going downward, it will not get latched.
	//
	// BWE-TODO: clean up this comment after implementing probing in TWCC case
	// NOTE: With TWCC, it is possible to reset bandwidth estimation to clean state as
	// the send side is in full control of bandwidth estimation.
	//
	channelObserverString := r.channelObserver.ToString()
	r.channelObserver = r.newChannelObserverNonProbe()
	r.params.Logger.Debugw(
		"probe done",
		"isNotFailing", isNotFailing,
		"isGoalReached", isGoalReached,
		"committedEstimate", r.committedChannelCapacity,
		"highestEstimate", highestEstimateInProbe,
		"channel", channelObserverString,
	)
	if !isNotFailing {
		return
	}

	if highestEstimateInProbe > r.committedChannelCapacity {
		r.committedChannelCapacity = highestEstimateInProbe
	}
}

func (r *RemoteBWE) GetProbeStatus() (bool, bwe.ChannelTrend, int64, int64) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if !r.isInProbe {
		return false, bwe.ChannelTrendNeutral, 0, 0
	}

	trend, _ := r.channelObserver.GetTrend()
	return r.channelObserver.HasEnoughEstimateSamples(),
		trend,
		r.channelObserver.GetLowestEstimate(),
		r.channelObserver.GetHighestEstimate()
}

func (r *RemoteBWE) worker() {
	ticker := time.NewTicker(r.params.Config.PeriodicCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.wake:
			r.lock.RLock()
			state := r.congestionState
			r.lock.RUnlock()
			if state == bwe.CongestionStateCongested {
				ticker.Reset(r.params.Config.PeriodicCheckIntervalCongested)
			} else {
				ticker.Reset(r.params.Config.PeriodicCheckInterval)
			}

		case <-ticker.C:
			var (
				shouldNotify             bool
				state                    bwe.CongestionState
				committedChannelCapacity int64
			)
			r.lock.Lock()
			shouldNotify, state, committedChannelCapacity = r.congestionDetectionStateMachine()
			r.lock.Unlock()

			if shouldNotify {
				if bweListener := r.getBWEListener(); bweListener != nil {
					bweListener.OnCongestionStateChange(state, committedChannelCapacity)
				}
			}

		case <-r.stop.Watch():
			return
		}
	}
}
