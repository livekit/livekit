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

	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/protocol/utils"
)

type MetricsCollectorProvider interface {
	MetricsCollectorTimeToCollectMetrics()
	MetricsCollectorBatchReady(mb *livekit.MetricsBatch)
}

// --------------------------------------------------------

type MetricsCollectorConfig struct {
	SamplingIntervalMs uint32 `yaml:"sampling_interval_ms,omitempty" json:"sampling_interval_ms,omitempty"`
	BatchIntervalMs    uint32 `yaml:"batch_interval_ms,omitempty" json:"batch_interval_ms,omitempty"`
}

var (
	DefaultMetricsCollectorConfig = MetricsCollectorConfig{
		SamplingIntervalMs: 3 * 1000,
		BatchIntervalMs:    10 * 1000,
	}
)

// --------------------------------------------------------

type MetricsCollectorParams struct {
	ParticipantIdentity livekit.ParticipantIdentity
	Config              MetricsCollectorConfig
	Provider            MetricsCollectorProvider
	Logger              logger.Logger
}

type MetricsCollector struct {
	params MetricsCollectorParams

	lock                  sync.RWMutex
	mbb                   *utils.MetricsBatchBuilder
	publisherRTTMetricId  map[livekit.ParticipantIdentity]int
	subscriberRTTMetricId int
	relayRTTMetricId      map[livekit.ParticipantIdentity]int

	stop core.Fuse
}

func NewMetricsCollector(params MetricsCollectorParams) *MetricsCollector {
	mc := &MetricsCollector{
		params: params,
	}
	mc.reset()

	go mc.worker()
	return mc
}

func (mc *MetricsCollector) Stop() {
	if mc != nil {
		mc.stop.Break()
	}
}

func (mc *MetricsCollector) AddPublisherRTT(participantIdentity livekit.ParticipantIdentity, rtt float32) {
	mc.lock.Lock()
	defer mc.lock.Unlock()

	metricId, ok := mc.publisherRTTMetricId[participantIdentity]
	if !ok {
		var err error
		metricId, err = mc.createTimeSeriesMetric(livekit.MetricLabel_PUBLISHER_RTT, participantIdentity)
		if err != nil {
			mc.params.Logger.Warnw("could not add time series metric for publisher RTT", err)
			return
		}

		mc.publisherRTTMetricId[participantIdentity] = metricId
	}

	mc.addTimeSeriesMetricSample(metricId, rtt)
}

func (mc *MetricsCollector) AddSubscriberRTT(rtt float32) {
	mc.lock.Lock()
	defer mc.lock.Unlock()

	if mc.subscriberRTTMetricId == utils.MetricsBatchBuilderInvalidTimeSeriesMetricId {
		var err error
		mc.subscriberRTTMetricId, err = mc.createTimeSeriesMetric(livekit.MetricLabel_SUBSCRIBER_RTT, mc.params.ParticipantIdentity)
		if err != nil {
			mc.params.Logger.Warnw("could not add time series metric for publisher RTT", err)
			return
		}
	}

	mc.addTimeSeriesMetricSample(mc.subscriberRTTMetricId, rtt)
}

func (mc *MetricsCollector) AddRelayRTT(participantIdentity livekit.ParticipantIdentity, rtt float32) {
	mc.lock.Lock()
	defer mc.lock.Unlock()

	metricId, ok := mc.relayRTTMetricId[participantIdentity]
	if !ok {
		var err error
		metricId, err = mc.createTimeSeriesMetric(livekit.MetricLabel_SERVER_MESH_RTT, participantIdentity)
		if err != nil {
			mc.params.Logger.Warnw("could not add time series metric for server mesh RTT", err)
			return
		}

		mc.relayRTTMetricId[participantIdentity] = metricId
	}

	mc.addTimeSeriesMetricSample(metricId, rtt)
}

func (mc *MetricsCollector) getMetricsBatchAndReset() *livekit.MetricsBatch {
	mc.lock.Lock()
	mbb := mc.mbb

	mc.reset()
	mc.lock.Unlock()

	if mbb.IsEmpty() {
		return nil
	}

	now := mono.Now()
	mbb.SetTime(now, now)
	return mbb.ToProto()
}

func (mc *MetricsCollector) reset() {
	mc.mbb = utils.NewMetricsBatchBuilder()
	mc.mbb.SetRestrictedLabels(utils.MetricRestrictedLabels{
		LabelRanges: []utils.MetricLabelRange{
			{
				StartInclusive: livekit.MetricLabel_CLIENT_VIDEO_SUBSCRIBER_FREEZE_COUNT,
				EndInclusive:   livekit.MetricLabel_CLIENT_VIDEO_PUBLISHER_QUALITY_LIMITATION_DURATION_OTHER,
			},
		},
		ParticipantIdentity: mc.params.ParticipantIdentity,
	})

	mc.publisherRTTMetricId = make(map[livekit.ParticipantIdentity]int)
	mc.subscriberRTTMetricId = utils.MetricsBatchBuilderInvalidTimeSeriesMetricId
	mc.relayRTTMetricId = make(map[livekit.ParticipantIdentity]int)
}

func (mc *MetricsCollector) createTimeSeriesMetric(
	label livekit.MetricLabel,
	participantIdentity livekit.ParticipantIdentity,
) (int, error) {
	return mc.mbb.AddTimeSeriesMetric(utils.TimeSeriesMetric{
		MetricLabel:         label,
		ParticipantIdentity: participantIdentity,
	},
	)
}

func (mc *MetricsCollector) addTimeSeriesMetricSample(metricId int, value float32) {
	now := mono.Now()
	if err := mc.mbb.AddMetricSamplesToTimeSeriesMetric(metricId, []utils.MetricSample{
		{
			At:           now,
			NormalizedAt: now,
			Value:        value,
		},
	}); err != nil {
		mc.params.Logger.Warnw("could not add metric sample", err, "metricId", metricId)
	}
}

func (mc *MetricsCollector) worker() {
	samplingIntervalMs := mc.params.Config.SamplingIntervalMs
	if samplingIntervalMs == 0 {
		samplingIntervalMs = DefaultMetricsCollectorConfig.SamplingIntervalMs
	}
	samplingTicker := time.NewTicker(time.Duration(samplingIntervalMs) * time.Millisecond)
	defer samplingTicker.Stop()

	batchIntervalMs := mc.params.Config.BatchIntervalMs
	if batchIntervalMs < samplingIntervalMs {
		batchIntervalMs = samplingIntervalMs
	}
	batchTicker := time.NewTicker(time.Duration(batchIntervalMs) * time.Millisecond)
	defer batchTicker.Stop()

	for {
		select {
		case <-samplingTicker.C:
			mc.params.Provider.MetricsCollectorTimeToCollectMetrics()

		case <-batchTicker.C:
			if mb := mc.getMetricsBatchAndReset(); mb != nil {
				mc.params.Provider.MetricsCollectorBatchReady(mb)
			}

		case <-mc.stop.Watch():
			return
		}
	}
}
