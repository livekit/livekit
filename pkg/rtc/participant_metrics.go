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
	"time"

	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/protocol/utils"
)

type ParticipantMetricsProvider interface {
	CollectMetrics()
	MetricsBatchReady(mb *livekit.MetricsBatch)
}

// --------------------------------------------------------

// RAJA-TODO: do JSON as well for use with feature flags.
type ParticipantMetricsConfig struct {
	SamplingInterval  time.Duration `yaml:"sampling_interval,omitempty"`
	ReportingInterval time.Duration `yaml:"reporting_interval,omitempty"`
}

var (
	DefaultParticipantMetricsConfig = ParticipantMetricsConfig{
		SamplingInterval:  3 * time.Second,
		ReportingInterval: 10 * time.Second,
	}
)

// --------------------------------------------------------

type ParticipantMetricsParams struct {
	ParticipantIdentity livekit.ParticipantIdentity
	Config              ParticipantMetricsConfig
	Provider            ParticipantMetricsProvider
	Logger              logger.Logger
}

type ParticipantMetrics struct {
	params ParticipantMetricsParams

	lock                  sync.RWMutex
	mbb                   *utils.MetricsBatchBuilder
	publisherRTTMetricId  map[livekit.ParticipantIdentity]int
	subscriberRTTMetricId int
	relayRTTMetricId      map[livekit.ParticipantIdentity]int

	stop core.Fuse
}

func NewParticipantMetrics(params ParticipantMetricsParams) *ParticipantMetrics {
	pm := &ParticipantMetrics{
		params: params,
	}
	pm.reset()

	go pm.worker()
	return pm
}

func (pm *ParticipantMetrics) Stop() {
	pm.stop.Break()
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

func (pm *ParticipantMetrics) Merge(other *livekit.MetricsBatch) {
	pm.lock.Lock()
	defer pm.lock.Unlock()

	pm.mbb.Merge(other)
}

func (pm *ParticipantMetrics) GetMetricsBatchAndReset() *livekit.MetricsBatch {
	pm.lock.Lock()
	mbb := pm.mbb

	pm.reset()
	pm.lock.Unlock()

	if mbb.IsEmpty() {
		return nil
	}

	now := mono.Now()
	mbb.SetTime(now, now)
	return mbb.ToProto()
}

func (pm *ParticipantMetrics) reset() {
	pm.mbb = utils.NewMetricsBatchBuilder()
	pm.mbb.SetRestrictedLabels(utils.MetricRestrictedLabels{
		LabelRanges: []utils.MetricLabelRange{
			{
				StartInclusive: livekit.MetricLabel_CLIENT_VIDEO_SUBSCRIBER_FREEZE_COUNT,
				EndInclusive:   livekit.MetricLabel_CLIENT_VIDEO_PUBLISHER_QUALITY_LIMITATION_DURATION_OTHER,
			},
		},
		ParticipantIdentity: pm.params.ParticipantIdentity,
	})

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

func (pm *ParticipantMetrics) worker() {
	samplingTicker := time.NewTicker(pm.params.Config.SamplingInterval)
	defer samplingTicker.Stop()

	reportingInterval := pm.params.Config.ReportingInterval
	if reportingInterval < pm.params.Config.SamplingInterval {
		reportingInterval = pm.params.Config.SamplingInterval
	}
	reportingTicker := time.NewTicker(reportingInterval)
	defer reportingTicker.Stop()

	for {
		select {
		case <-samplingTicker.C:
			pm.params.Provider.CollectMetrics()

		case <-reportingTicker.C:
			if mb := pm.GetMetricsBatchAndReset(); mb != nil {
				pm.params.Provider.MetricsBatchReady(mb)
			}

		case <-pm.stop.Watch():
			return
		}
	}
}
