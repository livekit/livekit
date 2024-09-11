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

package metric

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// ------------------------------------------------

type MetricTimestamperConfig struct {
	OneWayDelayEstimatorMinInterval time.Duration `yaml:"one_way_delay_estimator_min_interval,omitempty"`
	OneWayDelayEstimatorMaxBatch    int           `yaml:"one_way_delay_estimator_max_batch,omitempty"`
}

var (
	DefaultMetricTimestamperConfig = MetricTimestamperConfig{
		OneWayDelayEstimatorMinInterval: 5 * time.Second,
		OneWayDelayEstimatorMaxBatch:    100,
	}
)

// ------------------------------------------------

type MetricTimestamperParams struct {
	Config   MetricTimestamperConfig
	BaseTime time.Time
	Logger   logger.Logger
}

type MetricTimestamper struct {
	params                          MetricTimestamperParams
	lock                            sync.Mutex
	owdEstimator                    *utils.OWDEstimator
	lastOWDEstimatorRunAt           time.Time
	batchesSinceLastOWDEstimatorRun int
}

func NewMetricTimestamper(params MetricTimestamperParams) *MetricTimestamper {
	return &MetricTimestamper{
		params:                params,
		owdEstimator:          utils.NewOWDEstimator(utils.OWDEstimatorParamsDefault),
		lastOWDEstimatorRunAt: time.Now().Add(-params.Config.OneWayDelayEstimatorMinInterval),
	}
}

func (m *MetricTimestamper) Process(batch *livekit.MetricsBatch) {
	// run OWD estimation periodically
	estimatedOWD := m.maybeRunOWDEstimator(batch)

	// change all time stamps and add estimated OWD
	// NOTE: all timestamps will be re-mapped. If the time series or event happened some time
	// in the past and the OWD estimation has changed since, those samples will get the updated
	// OWD estimation applied. So, they may have more uncertainty in addition to the uncertainty
	// of OWD estimation process.
	// RAJA-TODO START
	batch.Timestamp = estimatedOWD.Nanoseconds()

	for _, ts := range batch.TimeSeries {
		for _, sample := range ts.Samples {
			sample.Timestamp = estimatedOWD.Nanoseconds()
		}
	}

	for _, ev := range batch.Events {
		ev.StartTimestamp = estimatedOWD.Nanoseconds()

		endTimestamp := ev.GetEndTimestamp()
		if endTimestamp != 0 {
			endTimestamp = estimatedOWD.Nanoseconds()
			ev.EndTimestamp = &endTimestamp
		}
	}
	// RAJA-TODO STOP
}

func (m *MetricTimestamper) maybeRunOWDEstimator(batch *livekit.MetricsBatch) time.Duration {
	m.lock.Lock()
	defer m.lock.Unlock()

	if time.Since(m.lastOWDEstimatorRunAt) < m.params.Config.OneWayDelayEstimatorMinInterval &&
		m.batchesSinceLastOWDEstimatorRun < m.params.Config.OneWayDelayEstimatorMaxBatch {
		m.batchesSinceLastOWDEstimatorRun++
		return m.owdEstimator.EstimatedPropagationDelay()
	}

	senderClockTime := batch.GetTimestamp()
	if senderClockTime == 0 {
		m.batchesSinceLastOWDEstimatorRun++
		return m.owdEstimator.EstimatedPropagationDelay()
	}

	m.lastOWDEstimatorRunAt = time.Now()
	m.batchesSinceLastOWDEstimatorRun = 1

	at := m.params.BaseTime.Add(time.Since(m.params.BaseTime))
	estimatedOWD, _ := m.owdEstimator.Update(time.UnixMilli(senderClockTime), at)
	return estimatedOWD
}
