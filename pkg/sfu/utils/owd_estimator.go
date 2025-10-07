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

package utils

import (
	"time"

	"go.uber.org/zap/zapcore"
)

type OWDEstimatorParams struct {
	PropagationDelayFallFactor float64
	PropagationDelayRiseFactor float64

	PropagationDelaySpikeAdaptationFactor float64

	PropagationDelayDeltaThresholdMin                time.Duration
	PropagationDelayDeltaThresholdMaxFactor          int64
	PropagationDelayDeltaHighResetNumReports         int
	PropagationDelayDeltaHighResetWait               time.Duration
	PropagationDelayDeltaLongTermAdaptationThreshold time.Duration
}

var OWDEstimatorParamsDefault = OWDEstimatorParams{
	// OWD (One-Way-Delay) Estimator is used to estimate propagation delay between sender and receiver.
	// As they operate on different clock domains, it is not possible to get exact propagation delay easily.
	// So, this module is an estimator using a simple approach explained below. It should not be used for
	// things that require high accuracy.
	//
	// One example is RTCP Sender Reports getting re-based to SFU time base so that all subscriber side
	// can have the same time base (i. e. SFU time base). To convert publisher side
	// RTCP Sender Reports to SFU timebase, a propagation delay is maintained.
	//    propagation_delay = time_of_report_reception - ntp_timestamp_in_report
	//
	// Propagation delay is adapted continuously. If it falls, adapt quickly to the
	// lower value as that could be the real propagation delay. If it rises, adapt slowly
	// as it might be a temporary change or slow drift. See below for handling of high deltas
	// which could be a result of a path change.
	PropagationDelayFallFactor: 0.9,
	PropagationDelayRiseFactor: 0.1,

	PropagationDelaySpikeAdaptationFactor: 0.5,

	// To account for path changes mid-stream, if the delta of the propagation delay is consistently higher, reset.
	// Reset at whichever of the below happens later.
	//   1. 10 seconds of persistent high delta.
	//   2. at least 2 consecutive reports with high delta.
	//
	// A long term estimate of delta of propagation delay is maintained and delta propagation delay exceeding
	// a factor of the long term estimate is considered a sharp increase. That will trigger the start of the
	// path change condition and if it persists, propagation delay will be reset.
	PropagationDelayDeltaThresholdMin:                10 * time.Millisecond,
	PropagationDelayDeltaThresholdMaxFactor:          2,
	PropagationDelayDeltaHighResetNumReports:         2,
	PropagationDelayDeltaHighResetWait:               10 * time.Second,
	PropagationDelayDeltaLongTermAdaptationThreshold: 50 * time.Millisecond,
}

type OWDEstimator struct {
	params OWDEstimatorParams

	initialized                        bool
	initialAdjustmentDone              bool
	lastSenderClockTimeNs              int64
	lastPropagationDelayNs             int64
	lastDeltaPropagationDelayNs        int64
	estimatedPropagationDelayNs        int64
	longTermDeltaPropagationDelayNs    int64
	propagationDelayDeltaHighCount     int
	propagationDelayDeltaHighStartTime time.Time
	propagationDelaySpikeNs            int64
}

func NewOWDEstimator(params OWDEstimatorParams) *OWDEstimator {
	return &OWDEstimator{
		params: params,
	}
}

func (o *OWDEstimator) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if o != nil {
		e.AddTime("lastSenderClockTimeNs", time.Unix(0, o.lastSenderClockTimeNs))
		e.AddDuration("lastPropagationDelayNs", time.Duration(o.lastPropagationDelayNs))
		e.AddDuration("lastDeltaPropagationDelayNs", time.Duration(o.lastDeltaPropagationDelayNs))
		e.AddDuration("estimatedPropagationDelayNs", time.Duration(o.estimatedPropagationDelayNs))
		e.AddDuration("longTermDeltaPropagationDelayNs", time.Duration(o.longTermDeltaPropagationDelayNs))
		e.AddInt("propagationDelayDeltaHighCount", o.propagationDelayDeltaHighCount)
		e.AddTime("propagationDelayDeltaHighStartTime", o.propagationDelayDeltaHighStartTime)
		e.AddDuration("propagationDelaySpikeNs", time.Duration(o.propagationDelaySpikeNs))
	}
	return nil
}

func (o *OWDEstimator) Update(senderClockTimeNs int64, receiverClockTimeNs int64) (int64, bool) {
	resetDelta := func() {
		o.propagationDelayDeltaHighCount = 0
		o.propagationDelayDeltaHighStartTime = time.Time{}
		o.propagationDelaySpikeNs = 0
	}

	initPropagationDelay := func(pd int64) {
		o.estimatedPropagationDelayNs = pd
		o.longTermDeltaPropagationDelayNs = 0
		resetDelta()
	}

	o.lastPropagationDelayNs = receiverClockTimeNs - senderClockTimeNs
	if !o.initialized {
		o.initialized = true
		o.lastSenderClockTimeNs = senderClockTimeNs
		initPropagationDelay(o.lastPropagationDelayNs)
		return o.estimatedPropagationDelayNs, true
	}

	stepChange := false
	o.lastDeltaPropagationDelayNs = o.lastPropagationDelayNs - o.estimatedPropagationDelayNs
	// check for path changes, i. e. a step jump increase in propagation delay observed over time
	if o.lastDeltaPropagationDelayNs > o.params.PropagationDelayDeltaThresholdMin.Nanoseconds() { // ignore small changes for path change consideration
		if o.longTermDeltaPropagationDelayNs != 0 &&
			o.lastDeltaPropagationDelayNs > o.longTermDeltaPropagationDelayNs*o.params.PropagationDelayDeltaThresholdMaxFactor {
			o.propagationDelayDeltaHighCount++
			if o.propagationDelayDeltaHighStartTime.IsZero() {
				o.propagationDelayDeltaHighStartTime = time.Now()
			}
			if o.propagationDelaySpikeNs == 0 {
				o.propagationDelaySpikeNs = o.lastPropagationDelayNs
			} else {
				o.propagationDelaySpikeNs += int64(o.params.PropagationDelaySpikeAdaptationFactor * float64(o.lastPropagationDelayNs-o.propagationDelaySpikeNs))
			}

			if o.propagationDelayDeltaHighCount >= o.params.PropagationDelayDeltaHighResetNumReports && time.Since(o.propagationDelayDeltaHighStartTime) >= o.params.PropagationDelayDeltaHighResetWait {
				stepChange = true
				initPropagationDelay(o.propagationDelaySpikeNs)
			}
		} else {
			resetDelta()
		}
	} else {
		resetDelta()

		factor := o.params.PropagationDelayFallFactor
		if o.lastPropagationDelayNs > o.estimatedPropagationDelayNs {
			factor = o.params.PropagationDelayRiseFactor
		}
		o.estimatedPropagationDelayNs += int64(factor * float64(o.lastPropagationDelayNs-o.estimatedPropagationDelayNs))
	}

	if o.lastDeltaPropagationDelayNs < o.params.PropagationDelayDeltaLongTermAdaptationThreshold.Nanoseconds() {
		if o.longTermDeltaPropagationDelayNs == 0 {
			o.longTermDeltaPropagationDelayNs = o.lastDeltaPropagationDelayNs
		} else {
			// do not adapt to large +ve spikes, can happen when channel is congested and reports are delivered very late
			// if the spike is in fact a path change, it will persist and handled by path change detection above
			sinceLast := senderClockTimeNs - o.lastSenderClockTimeNs
			adaptationFactor := min(1.0, float64(sinceLast)/float64(o.params.PropagationDelayDeltaHighResetWait))
			o.longTermDeltaPropagationDelayNs += int64(adaptationFactor * float64(o.lastDeltaPropagationDelayNs-o.longTermDeltaPropagationDelayNs))
		}
	}
	if o.longTermDeltaPropagationDelayNs < 0 {
		o.longTermDeltaPropagationDelayNs = 0
	}
	o.lastSenderClockTimeNs = senderClockTimeNs
	return o.estimatedPropagationDelayNs, stepChange
}

func (o *OWDEstimator) InitialAdjustment(adjustmentNs int64) int64 {
	if o.initialAdjustmentDone {
		return o.estimatedPropagationDelayNs
	}

	o.initialAdjustmentDone = true
	// one time adjustment at init
	// example: when this is used to measure one-way-delay of RTCP sender reports,
	// it is possible that the first sender report is delayed and experiences more
	// than existing propagation delay. This allows adjustment of initial estimate.
	if adjustmentNs < 0 && -adjustmentNs < o.estimatedPropagationDelayNs {
		o.estimatedPropagationDelayNs += adjustmentNs
	}
	return o.estimatedPropagationDelayNs
}

func (o *OWDEstimator) EstimatedPropagationDelay() int64 {
	return o.estimatedPropagationDelayNs
}
