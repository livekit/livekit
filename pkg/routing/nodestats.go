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

package routing

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

type NodeStats struct {
	config    config.NodeStatsConfig
	startedAt int64

	lock                 sync.Mutex
	statsHistory         []*livekit.NodeStats
	statsHistoryWritePtr int
}

func NewNodeStats(conf *config.NodeStatsConfig, startedAt int64) *NodeStats {
	n := &NodeStats{
		startedAt: startedAt,
	}
	n.UpdateConfig(conf)
	return n
}

func (n *NodeStats) UpdateConfig(conf *config.NodeStatsConfig) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if conf == nil {
		conf = &config.DefaultNodeStatsConfig
	}
	n.config = *conf

	// set up stats history to be able to measure different rate windows
	var maxInterval time.Duration
	for _, rateInterval := range conf.StatsRateMeasurementIntervals {
		if rateInterval > maxInterval {
			maxInterval = rateInterval
		}
	}
	n.statsHistory = make([]*livekit.NodeStats, (maxInterval+conf.StatsUpdateInterval-1)/conf.StatsUpdateInterval)
	n.statsHistoryWritePtr = 0
}

func (n *NodeStats) UpdateAndGetNodeStats() (*livekit.NodeStats, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	stats, err := prometheus.GetNodeStats(
		n.startedAt,
		append(n.statsHistory[n.statsHistoryWritePtr:], n.statsHistory[0:n.statsHistoryWritePtr]...),
		n.config.StatsRateMeasurementIntervals,
	)
	if err != nil {
		logger.Errorw("could not update node stats", err)
		return nil, err
	}

	n.statsHistory[n.statsHistoryWritePtr] = stats
	n.statsHistoryWritePtr = (n.statsHistoryWritePtr + 1) % len(n.statsHistory)
	return stats, nil
}
