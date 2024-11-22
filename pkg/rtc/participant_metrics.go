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

package rtc

import (
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/protocol/utils"
)

type ParticipantMetricsParams struct {
	ParticipantIdentity livekit.ParticipantIdentity
	Logger              logger.Logger
}

type ParticipantMetrics struct {
	params ParticipantMetricsParams

	lock                  sync.RWMutex
	mbb                   *utils.MetricsBatchBuilder
	publisherRTTMetricId  map[livekit.ParticipantIdentity]int
	subscriberRTTMetricId int
	relayRTTMetricId      map[livekit.ParticipantIdentity]int
}

func NewParticipantMetrics(params ParticipantMetricsParams) *ParticipantMetrics {
	pm := &ParticipantMetrics{
		params: params,
	}
	pm.reset()
	return pm
}

func (pm *ParticipantMetrics) AddPublisherRTT(participantIdentity livekit.ParticipantIdentity, rtt float32) {
	pm.lock.Lock()
	defer pm.lock.Unlock()

	metricId, ok := pm.publisherRTTMetricId[participantIdentity]
	if !ok {
		var err error
		metricId, err = pm.createTimeSeriesMetric(livekit.MetricLabel_PUBLISHER_RTT, participantIdentity)
		if err != nil {
			pm.params.Logger.Warnw("could not add time series metric for publisher RTT", err)
			return
		}

		pm.publisherRTTMetricId[participantIdentity] = metricId
	}

	pm.addTimeSeriesMetricSample(metricId, rtt)
}

func (pm *ParticipantMetrics) AddSubscriberRTT(rtt float32) {
	pm.lock.Lock()
	defer pm.lock.Unlock()

	if pm.subscriberRTTMetricId == utils.MetricsBatchBuilderInvalidTimeSeriesMetricId {
		var err error
		pm.subscriberRTTMetricId, err = pm.createTimeSeriesMetric(livekit.MetricLabel_SUBSCRIBER_RTT, pm.params.ParticipantIdentity)
		if err != nil {
			pm.params.Logger.Warnw("could not add time series metric for publisher RTT", err)
			return
		}
	}

	pm.addTimeSeriesMetricSample(pm.subscriberRTTMetricId, rtt)
}

func (pm *ParticipantMetrics) AddRelayRTT(participantIdentity livekit.ParticipantIdentity, rtt float32) {
	pm.lock.Lock()
	defer pm.lock.Unlock()

	metricId, ok := pm.relayRTTMetricId[participantIdentity]
	if !ok {
		var err error
		metricId, err = pm.createTimeSeriesMetric(livekit.MetricLabel_PUBLISHER_RTT, participantIdentity)
		if err != nil {
			pm.params.Logger.Warnw("could not add time series metric for publisher RTT", err)
			return
		}

		pm.relayRTTMetricId[participantIdentity] = metricId
	}

	pm.addTimeSeriesMetricSample(metricId, rtt)
}

func (pm *ParticipantMetrics) GetMetricsBatch() *livekit.MetricsBatch {
	pm.lock.Lock()
	mbb := pm.mbb

	pm.reset()
	pm.lock.Unlock()

	now := mono.Now()
	mbb.SetTime(now, now)
	return mbb.ToProto()
}

func (pm *ParticipantMetrics) reset() {
	pm.mbb = utils.NewMetricsBatchBuilder()
	pm.publisherRTTMetricId = make(map[livekit.ParticipantIdentity]int)
	pm.subscriberRTTMetricId = utils.MetricsBatchBuilderInvalidTimeSeriesMetricId
	pm.relayRTTMetricId = make(map[livekit.ParticipantIdentity]int)
}

func (pm *ParticipantMetrics) createTimeSeriesMetric(
	label livekit.MetricLabel,
	participantIdentity livekit.ParticipantIdentity,
) (int, error) {
	return pm.mbb.AddTimeSeriesMetric(utils.TimeSeriesMetric{
		MetricLabel:         label,
		ParticipantIdentity: participantIdentity,
	},
	)
}

func (pm *ParticipantMetrics) addTimeSeriesMetricSample(metricId int, value float32) {
	now := mono.Now()
	if err := pm.mbb.AddMetricSamplesToTimeSeriesMetric(metricId, []utils.MetricSample{
		{
			At:           now,
			NormalizedAt: now,
			Value:        value,
		},
	}); err != nil {
		pm.params.Logger.Warnw("could not add metric sample", err, "metricId", metricId)
	}
}
