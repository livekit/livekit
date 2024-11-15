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
	"github.com/gammazero/deque"
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

type rembReport struct {
	at                     time.Time
	receivedEstimate       int64
	isInProbe              bool // RAJA-TODO-BWE: this should not be here
	isProbeFinalizing      bool // RAJA-TODO-BWE: this should not be here
	expectedBandwidthUsage int64
	sentPackets            uint32
	repeatedNacks          uint32
}

type RemoteBWEParams struct {
	Config RemoteBWEConfig
	Logger logger.Logger
}

type RemoteBWE struct {
	bwe.NullBWE

	params RemoteBWEParams

	lock        sync.RWMutex
	rembReports deque.Deque[rembReport]

	lastReceivedEstimate       int64
	lastExpectedBandwidthUsage int64
	committedChannelCapacity   int64

	channelObserver *channelObserver

	congestionState           bwe.CongestionState
	congestionStateSwitchedAt time.Time

	wake chan struct{}
	stop core.Fuse

	bweListener bwe.BWEListener
}

func NewRemoteBWE(params RemoteBWEParams) *RemoteBWE {
	return &RemoteBWE{
		params: params,
	}
}

func (r *RemoteBWE) SetBWEListener(bweListener bwe.BWEListener) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.bweListener = bweListener
}

// RAJA-TODO-BWE: is this interface neeeded?
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
	isInProbe bool,
	isProbeFinalizing bool,
	expectedBandwidthUsage int64,
	sentPackets uint32,
	repeatedNacks uint32,
) {
	r.lock.Lock()
	r.rembReports.PushBack(rembReport{
		mono.Now(),
		receivedEstimate,
		isInProbe,
		isProbeFinalizing,
		expectedBandwidthUsage,
		sentPackets,
		repeatedNacks,
	})
	r.lock.Unlock()

	// notify worker of a new REMB
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// RAJA-TODO-BWE: this API should not be necessary - should be all callbacks from here
func (r *RemoteBWE) GetCongestionState() bwe.CongestionState {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.congestionState
}

func (r *RemoteBWE) congestionDetectionStateMachine() {
	state := r.GetCongestionState()
	newState := state
	update := false

	trend, reason := r.channelObserver.GetTrend()
	switch state {
	case bwe.CongestionStateNone:
		if trend == ChannelTrendCongesting {
			if r.estimateAvailableChannelCapacity(reason) {
				newState = bwe.CongestionStateCongested
			}
		}

	case bwe.CongestionStateCongested:
		if trend == ChannelTrendCongesting {
			if r.estimateAvailableChannelCapacity(reason) {
				// update state sa this needs to reset switch time to wait for congestion min duration again
				update = true
			}
		} else if time.Since(r.congestionStateSwitchedAt) >= r.params.Config.CongestedMinDuration {
			newState = bwe.CongestionStateNone
		}
	}

	if newState != state || update {
		r.updateCongestionState(newState, reason)
	}
}

// RAJA-TODO-BWE: this API should not be necessary - should be all callbacks from here
func (r *RemoteBWE) GetEstimatedAvailableChannelCapacity() int64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.committedChannelCapacity
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

	r.lock.Lock()
	r.committedChannelCapacity = estimateToCommit
	r.lock.Unlock()

	// reset to get new set of samples for next trend
	r.channelObserver = r.newChannelObserverNonProbe()
	return true
}

func (r *RemoteBWE) processREMBReport(report rembReport) {
	r.lastReceivedEstimate = report.receivedEstimate
	r.lastExpectedBandwidthUsage = report.expectedBandwidthUsage

	if !report.isProbeFinalizing {
		r.channelObserver.AddEstimate(r.lastReceivedEstimate)
		r.channelObserver.AddNack(report.sentPackets, report.repeatedNacks)
	}

	if !report.isInProbe { // RAJA-TODO-BWE: avoid this check, state machine should run always, state machine can probably manage in-probe/non-probe logic
		r.congestionDetectionStateMachine()
	}
}

func (r *RemoteBWE) updateCongestionState(state bwe.CongestionState, reason channelCongestionReason) {
	r.lock.Lock()
	r.params.Logger.Infow(
		"congestion state change",
		"from", r.congestionState,
		"to", state,
		"reason", reason,
		"committedChannelCapacity", r.committedChannelCapacity,
	)

	r.congestionState = state
	r.congestionStateSwitchedAt = mono.Now()

	bweListener := r.bweListener
	committedChannelCapacity := r.committedChannelCapacity
	r.lock.Unlock()

	if bweListener != nil {
		bweListener.OnCongestionStateChange(state, committedChannelCapacity)
	}
}

// RAJA-TODO-BWE: need to have InitProbe OR ProbingStart interface where this needs to be instantiated
func (r *RemoteBWE) newChannelObserverProbe() *channelObserver {
	return newChannelObserver(
		channelObserverParams{
			Name:   "probe",
			Config: r.params.Config.ChannelObserverProbe,
		},
		r.params.Logger,
	)
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

func (r *RemoteBWE) ProbingStart() {
	channelState := ""
	if r.channelObserver != nil {
		channelState = r.channelObserver.ToString()
	}
	r.channelObserver = r.newChannelObserverProbe()
	r.channelObserver.SeedEstimate(r.lastReceivedEstimate)

	r.params.Logger.Debugw(
		"stream allocator: starting probe",
		"lastReceived", r.lastReceivedEstimate,
		"channel", channelState,
	)
}

func (r *RemoteBWE) worker() {
	ticker := time.NewTicker(r.params.Config.PeriodicCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.wake:
			for {
				r.lock.Lock()
				if r.rembReports.Len() == 0 {
					r.lock.Unlock()
					break
				}
				rembReport := r.rembReports.PopFront()
				r.lock.Unlock()

				r.processREMBReport(rembReport)
			}

			if r.GetCongestionState() == bwe.CongestionStateCongested {
				ticker.Reset(r.params.Config.PeriodicCheckIntervalCongested)
			} else {
				ticker.Reset(r.params.Config.PeriodicCheckInterval)
			}

		case <-ticker.C:
			r.congestionDetectionStateMachine()

		case <-r.stop.Watch():
			return
		}
	}
}
