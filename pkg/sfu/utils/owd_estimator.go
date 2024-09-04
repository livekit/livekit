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
	PropagationDelayDeltaThresholdMaxFactor          int
	PropagationDelayDeltaHighResetNumReports         int
	PropagationDelayDeltaHighResetWait               time.Duration
	PropagationDelayDeltaLongTermAdaptationThreshold time.Duration
}

var OWDEstimatorParamsDefault = OWDEstimatorParams{
	// OWD (One-Way-Delay) Estimator is used to estimate propagation delay between sender and receicer.
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
	lastSenderClockTime                time.Time
	lastPropagationDelay               time.Duration
	lastDeltaPropagationDelay          time.Duration
	estimatedPropagationDelay          time.Duration
	longTermDeltaPropagationDelay      time.Duration
	propagationDelayDeltaHighCount     int
	propagationDelayDeltaHighStartTime time.Time
	propagationDelaySpike              time.Duration
}

func NewOWDEstimator(params OWDEstimatorParams) *OWDEstimator {
	return &OWDEstimator{
		params: params,
	}
}

func (o *OWDEstimator) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if o != nil {
		e.AddTime("lastSenderClockTime", o.lastSenderClockTime)
		e.AddDuration("lastPropagationDelay", o.lastPropagationDelay)
		e.AddDuration("lastDeltaPropagationDelay", o.lastDeltaPropagationDelay)
		e.AddDuration("estimatedPropagationDelay", o.estimatedPropagationDelay)
		e.AddDuration("longTermDeltaPropagationDelay", o.longTermDeltaPropagationDelay)
		e.AddInt("propagationDelayDeltaHighCount", o.propagationDelayDeltaHighCount)
		e.AddTime("propagationDelayDeltaHighStartTime", o.propagationDelayDeltaHighStartTime)
		e.AddDuration("propagationDelaySpike", o.propagationDelaySpike)
	}
	return nil
}

func (o *OWDEstimator) Update(senderClockTime time.Time, receiverClockTime time.Time) (time.Duration, bool) {
	resetDelta := func() {
		o.propagationDelayDeltaHighCount = 0
		o.propagationDelayDeltaHighStartTime = time.Time{}
		o.propagationDelaySpike = 0
	}

	initPropagationDelay := func(pd time.Duration) {
		o.estimatedPropagationDelay = pd
		o.longTermDeltaPropagationDelay = 0
		resetDelta()
	}

	o.lastPropagationDelay = receiverClockTime.Sub(senderClockTime)
	if !o.initialized {
		o.initialized = true
		o.lastSenderClockTime = senderClockTime
		initPropagationDelay(o.lastPropagationDelay)
		return o.estimatedPropagationDelay, true
	}

	stepChange := false
	o.lastDeltaPropagationDelay = o.lastPropagationDelay - o.estimatedPropagationDelay
	// check for path changes, i. e. a step jump increase in propagation delay observed over time
	if o.lastDeltaPropagationDelay > o.params.PropagationDelayDeltaThresholdMin { // ignore small changes for path change consideration
		if o.longTermDeltaPropagationDelay != 0 &&
			o.lastDeltaPropagationDelay > o.longTermDeltaPropagationDelay*time.Duration(o.params.PropagationDelayDeltaThresholdMaxFactor) {
			o.propagationDelayDeltaHighCount++
			if o.propagationDelayDeltaHighStartTime.IsZero() {
				o.propagationDelayDeltaHighStartTime = time.Now()
			}
			if o.propagationDelaySpike == 0 {
				o.propagationDelaySpike = o.lastPropagationDelay
			} else {
				o.propagationDelaySpike += time.Duration(o.params.PropagationDelaySpikeAdaptationFactor * float64(o.lastPropagationDelay-o.propagationDelaySpike))
			}

			if o.propagationDelayDeltaHighCount >= o.params.PropagationDelayDeltaHighResetNumReports && time.Since(o.propagationDelayDeltaHighStartTime) >= o.params.PropagationDelayDeltaHighResetWait {
				stepChange = true
				initPropagationDelay(o.propagationDelaySpike)
			}
		} else {
			resetDelta()
		}
	} else {
		resetDelta()

		factor := o.params.PropagationDelayFallFactor
		if o.lastPropagationDelay > o.estimatedPropagationDelay {
			factor = o.params.PropagationDelayRiseFactor
		}
		o.estimatedPropagationDelay += time.Duration(factor * float64(o.lastPropagationDelay-o.estimatedPropagationDelay))
	}

	if o.lastDeltaPropagationDelay < o.params.PropagationDelayDeltaLongTermAdaptationThreshold {
		if o.longTermDeltaPropagationDelay == 0 {
			o.longTermDeltaPropagationDelay = o.lastDeltaPropagationDelay
		} else {
			// do not adapt to large +ve spikes, can happen when channel is congested and reports are delivered very late
			// if the spike is in fact a path change, it will persist and handled by path change detection above
			sinceLast := senderClockTime.Sub(o.lastSenderClockTime)
			adaptationFactor := min(1.0, float64(sinceLast)/float64(o.params.PropagationDelayDeltaHighResetWait))
			o.longTermDeltaPropagationDelay += time.Duration(adaptationFactor * float64(o.lastDeltaPropagationDelay-o.longTermDeltaPropagationDelay))
		}
	}
	if o.longTermDeltaPropagationDelay < 0 {
		o.longTermDeltaPropagationDelay = 0
	}
	o.lastSenderClockTime = senderClockTime
	return o.estimatedPropagationDelay, stepChange
}
