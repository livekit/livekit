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

type MetricsReporterConsumer interface {
	MetricsReporterBatchReady(mb *livekit.MetricsBatch)
}

// --------------------------------------------------------

type MetricsReporterConfig struct {
	ReportingIntervalMs uint32 `yaml:"reporting_interval_ms,omitempty" json:"reporting_interval_ms,omitempty"`
}

var (
	DefaultMetricsReporterConfig = MetricsReporterConfig{
		ReportingIntervalMs: 10 * 1000,
	}
)

// --------------------------------------------------------

type MetricsReporterParams struct {
	ParticipantIdentity livekit.ParticipantIdentity
	Config              MetricsReporterConfig
	Consumer            MetricsReporterConsumer
	Logger              logger.Logger
}

type MetricsReporter struct {
	params MetricsReporterParams

	lock sync.RWMutex
	mbb  *utils.MetricsBatchBuilder

	stop core.Fuse
}

func NewMetricsReporter(params MetricsReporterParams) *MetricsReporter {
	mr := &MetricsReporter{
		params: params,
	}
	mr.reset()

	go mr.worker()
	return mr
}

func (mr *MetricsReporter) Stop() {
	if mr != nil {
		mr.stop.Break()
	}
}

func (mr *MetricsReporter) Merge(other *livekit.MetricsBatch) {
	if mr == nil {
		return
	}

	mr.lock.Lock()
	defer mr.lock.Unlock()

	mr.mbb.Merge(other)
}

func (mr *MetricsReporter) getMetricsBatchAndReset() *livekit.MetricsBatch {
	mr.lock.Lock()
	mbb := mr.mbb

	mr.reset()
	mr.lock.Unlock()

	if mbb.IsEmpty() {
		return nil
	}

	now := mono.Now()
	mbb.SetTime(now, now)
	return mbb.ToProto()
}

func (mr *MetricsReporter) reset() {
	mr.mbb = utils.NewMetricsBatchBuilder()
	mr.mbb.SetRestrictedLabels(utils.MetricRestrictedLabels{
		LabelRanges: []utils.MetricLabelRange{
			{
				StartInclusive: livekit.MetricLabel_CLIENT_VIDEO_SUBSCRIBER_FREEZE_COUNT,
				EndInclusive:   livekit.MetricLabel_CLIENT_VIDEO_PUBLISHER_QUALITY_LIMITATION_DURATION_OTHER,
			},
		},
		ParticipantIdentity: mr.params.ParticipantIdentity,
	})
}

func (mr *MetricsReporter) worker() {
	reportingIntervalMs := mr.params.Config.ReportingIntervalMs
	if reportingIntervalMs == 0 {
		reportingIntervalMs = DefaultMetricsReporterConfig.ReportingIntervalMs
	}
	reportingTicker := time.NewTicker(time.Duration(reportingIntervalMs) * time.Millisecond)
	defer reportingTicker.Stop()

	for {
		select {
		case <-reportingTicker.C:
			if mb := mr.getMetricsBatchAndReset(); mb != nil {
				mr.params.Consumer.MetricsReporterBatchReady(mb)
			}

		case <-mr.stop.Watch():
			return
		}
	}
}
