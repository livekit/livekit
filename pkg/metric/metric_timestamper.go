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
	"github.com/livekit/protocol/utils/mono"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	Config MetricTimestamperConfig
	Logger logger.Logger
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
	if m == nil {
		return
	}

	// run OWD estimation periodically
	estimatedOWDNanos := m.maybeRunOWDEstimator(batch)

	// normalize all time stamps and add estimated OWD
	// NOTE: all timestamps will be re-mapped. If the time series or event happened some time
	// in the past and the OWD estimation has changed since, those samples will get the updated
	// OWD estimation applied. So, they may have more uncertainty in addition to the uncertainty
	// of OWD estimation process.
	batch.NormalizedTimestamp = timestamppb.New(time.Unix(0, batch.TimestampMs*1e6+estimatedOWDNanos))

	for _, ts := range batch.TimeSeries {
		for _, sample := range ts.Samples {
			sample.NormalizedTimestamp = timestamppb.New(time.Unix(0, sample.TimestampMs*1e6+estimatedOWDNanos))
		}
	}

	for _, ev := range batch.Events {
		ev.NormalizedStartTimestamp = timestamppb.New(time.Unix(0, ev.StartTimestampMs*1e6+estimatedOWDNanos))

		endTimestampMs := ev.GetEndTimestampMs()
		if endTimestampMs != 0 {
			ev.NormalizedEndTimestamp = timestamppb.New(time.Unix(0, endTimestampMs*1e6+estimatedOWDNanos))
		}
	}

	m.params.Logger.Debugw("timestamped metrics batch", "batch", logger.Proto(batch))
}

func (m *MetricTimestamper) maybeRunOWDEstimator(batch *livekit.MetricsBatch) int64 {
	m.lock.Lock()
	defer m.lock.Unlock()

	if time.Since(m.lastOWDEstimatorRunAt) < m.params.Config.OneWayDelayEstimatorMinInterval &&
		m.batchesSinceLastOWDEstimatorRun < m.params.Config.OneWayDelayEstimatorMaxBatch {
		m.batchesSinceLastOWDEstimatorRun++
		return m.owdEstimator.EstimatedPropagationDelay()
	}

	senderClockTime := batch.GetTimestampMs()
	if senderClockTime == 0 {
		m.batchesSinceLastOWDEstimatorRun++
		return m.owdEstimator.EstimatedPropagationDelay()
	}

	m.lastOWDEstimatorRunAt = time.Now()
	m.batchesSinceLastOWDEstimatorRun = 1

	estimatedOWDNs, _ := m.owdEstimator.Update(senderClockTime*1e6, mono.UnixNano())
	return estimatedOWDNs
}
