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
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

var _ bwe.BWE = (*RemoteBWE)(nil)

// ---------------------------------------------------------------------------

type RemoteBWEConfig struct {
	NackRatioAttenuator     float64               `yaml:"nack_ratio_attenuator,omitempty"`
	ExpectedUsageThreshold  float64               `yaml:"expected_usage_threshold,omitempty"`
	ChannelObserverProbe    ChannelObserverConfig `yaml:"channel_observer_probe,omitempty"`
	ChannelObserverNonProbe ChannelObserverConfig `yaml:"channel_observer_non_probe,omitempty"`
	ProbeController         ProbeControllerConfig `yaml:"probe_controller,omitempty"`
}

var (
	DefaultRemoteBWEConfig = RemoteBWEConfig{
		NackRatioAttenuator:     0.4,
		ExpectedUsageThreshold:  0.95,
		ChannelObserverProbe:    defaultChannelObserverConfigProbe,
		ChannelObserverNonProbe: defaultChannelObserverConfigNonProbe,
		ProbeController:         DefaultProbeControllerConfig,
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
	committedChannelCapacity   int64

	probeController *probeController

	channelObserver *channelObserver

	congestionState           bwe.CongestionState
	congestionStateSwitchedAt time.Time

	bweListener bwe.BWEListener
}

func NewRemoteBWE(params RemoteBWEParams) *RemoteBWE {
	r := &RemoteBWE{
		params: params,
	}

	r.Reset()
	return r
}

func (r *RemoteBWE) Type() bwe.BWEType {
	return bwe.BWETypeRemote
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

	r.lastReceivedEstimate = 0
	r.lastExpectedBandwidthUsage = 0
	r.committedChannelCapacity = 100_000_000

	r.congestionState = bwe.CongestionStateNone
	r.congestionStateSwitchedAt = mono.Now()

	r.probeController = newProbeController(probeControllerParams{
		Config: r.params.Config.ProbeController,
		Logger: r.params.Logger,
	})

	r.newChannelObserver()
}

func (r *RemoteBWE) HandleREMB(
	receivedEstimate int64,
	expectedBandwidthUsage int64,
	sentPackets uint32,
	repeatedNacks uint32,
) {
	r.lock.Lock()
	r.lastReceivedEstimate = receivedEstimate
	r.lastExpectedBandwidthUsage = expectedBandwidthUsage

	// in probe, freeze channel observer state if probe causes congestion till the probe is done,
	// this is to ensure that probe result is not marked a success,
	// an unsuccessful probe will not up allocate any tracks
	if r.congestionState != bwe.CongestionStateNone && r.probeController.IsInProbe() {
		r.lock.Unlock()
		return
	}

	r.channelObserver.AddEstimate(r.lastReceivedEstimate)
	r.channelObserver.AddNack(sentPackets, repeatedNacks)

	shouldNotify, fromState, toState, committedChannelCapacity := r.congestionDetectionStateMachine()
	r.lock.Unlock()

	if shouldNotify {
		if bweListener := r.getBWEListener(); bweListener != nil {
			bweListener.OnCongestionStateChange(fromState, toState, committedChannelCapacity)
		}
	}
}

func (r *RemoteBWE) UpdateRTT(rtt float64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.probeController.UpdateRTT(rtt)
}

func (r *RemoteBWE) congestionDetectionStateMachine() (bool, bwe.CongestionState, bwe.CongestionState, int64) {
	fromState := r.congestionState
	toState := r.congestionState
	update := false
	trend, reason := r.channelObserver.GetTrend()

	switch fromState {
	case bwe.CongestionStateNone:
		if trend == channelTrendCongesting {
			if r.probeController.IsInProbe() || r.estimateAvailableChannelCapacity(reason) {
				// when in probe, if congested, stays there till probe is done,
				// the estimate stays at pre-probe level
				toState = bwe.CongestionStateCongested
			}
		}

	case bwe.CongestionStateCongested:
		if trend == channelTrendCongesting {
			if r.estimateAvailableChannelCapacity(reason) {
				// update state as this needs to reset switch time to wait for congestion min duration again
				update = true
			}
		} else {
			toState = bwe.CongestionStateNone
		}
	}

	shouldNotify := false
	if toState != fromState || update {
		fromState, toState = r.updateCongestionState(toState, reason)
		shouldNotify = true
	}

	return shouldNotify, fromState, toState, r.committedChannelCapacity
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
	if estimateToCommit > commitThreshold || r.committedChannelCapacity == estimateToCommit {
		return false
	}

	r.params.Logger.Infow(
		"remote bwe: channel congestion detected, applying channel capacity update",
		"reason", reason,
		"old(bps)", r.committedChannelCapacity,
		"new(bps)", estimateToCommit,
		"lastReceived(bps)", r.lastReceivedEstimate,
		"expectedUsage(bps)", r.lastExpectedBandwidthUsage,
		"commitThreshold(bps)", commitThreshold,
		"channel", r.channelObserver,
	)
	r.committedChannelCapacity = estimateToCommit
	return true
}

func (r *RemoteBWE) updateCongestionState(state bwe.CongestionState, reason channelCongestionReason) (bwe.CongestionState, bwe.CongestionState) {
	r.params.Logger.Debugw(
		"remote bwe: congestion state change",
		"from", r.congestionState,
		"to", state,
		"reason", reason,
		"committedChannelCapacity", r.committedChannelCapacity,
	)

	fromState := r.congestionState
	r.congestionState = state
	r.congestionStateSwitchedAt = mono.Now()
	return fromState, r.congestionState
}

func (r *RemoteBWE) CongestionState() bwe.CongestionState {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.congestionState
}

func (r *RemoteBWE) CanProbe() bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.congestionState == bwe.CongestionStateNone && r.probeController.CanProbe()
}

func (r *RemoteBWE) ProbeDuration() time.Duration {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.probeController.ProbeDuration()
}

func (r *RemoteBWE) ProbeClusterStarting(pci ccutils.ProbeClusterInfo) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.lastExpectedBandwidthUsage = int64(pci.Goal.ExpectedUsageBps)

	r.params.Logger.Debugw(
		"remote bwe: starting probe",
		"lastReceived", r.lastReceivedEstimate,
		"expectedBandwidthUsage", r.lastExpectedBandwidthUsage,
		"channel", r.channelObserver,
	)

	r.probeController.ProbeClusterStarting(pci)
	r.newChannelObserver()
}

func (r *RemoteBWE) ProbeClusterDone(pci ccutils.ProbeClusterInfo) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.probeController.ProbeClusterDone(pci)
}

func (r *RemoteBWE) ProbeClusterIsGoalReached() bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.probeController.IsInProbe() ||
		r.congestionState != bwe.CongestionStateNone ||
		!r.channelObserver.HasEnoughEstimateSamples() {
		return false
	}

	return r.probeController.ProbeClusterIsGoalReached(r.channelObserver.GetHighestEstimate())
}

func (r *RemoteBWE) ProbeClusterFinalize() (ccutils.ProbeSignal, int64, bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	pci, isFinalized := r.probeController.MaybeFinalizeProbe()
	if !isFinalized {
		return ccutils.ProbeSignalInconclusive, 0, isFinalized
	}

	// switch to a non-probe channel observer on probe end,
	// reset congestion state to get a fresh trend
	pco := r.channelObserver
	probeCongestionState := r.congestionState

	r.congestionState = bwe.CongestionStateNone
	r.newChannelObserver()

	r.params.Logger.Infow(
		"remote bwe: probe finalized",
		"lastReceived", r.lastReceivedEstimate,
		"expectedBandwidthUsage", r.lastExpectedBandwidthUsage,
		"channel", pco,
		"isSignalValid", pco.HasEnoughEstimateSamples(),
		"probeClusterInfo", pci,
		"rtt", r.probeController.GetRTT(),
	)

	probeSignal := ccutils.ProbeSignalNotCongesting
	if probeCongestionState != bwe.CongestionStateNone {
		probeSignal = ccutils.ProbeSignalCongesting
	} else if !pco.HasEnoughEstimateSamples() {
		probeSignal = ccutils.ProbeSignalInconclusive
	} else {
		highestEstimate := pco.GetHighestEstimate()
		if highestEstimate > r.committedChannelCapacity {
			r.committedChannelCapacity = highestEstimate
		}
	}

	r.probeController.ProbeSignal(probeSignal, pci.CreatedAt)
	return probeSignal, r.committedChannelCapacity, true
}

func (r *RemoteBWE) newChannelObserver() {
	var params channelObserverParams
	if r.probeController.IsInProbe() {
		params = channelObserverParams{
			Name:   "probe",
			Config: r.params.Config.ChannelObserverProbe,
		}
	} else {
		params = channelObserverParams{
			Name:   "non-probe",
			Config: r.params.Config.ChannelObserverNonProbe,
		}
	}

	r.channelObserver = newChannelObserver(params, r.params.Logger)
}
