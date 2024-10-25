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
	"runtime"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

type LocalNode struct {
	lock sync.RWMutex
	node *livekit.Node

	// previous stats for computing averages
	prevStats *livekit.NodeStats
}

func NewLocalNode(conf *config.Config) (*LocalNode, error) {
	nodeID := guid.New(utils.NodePrefix)
	if conf.RTC.NodeIP == "" {
		return nil, ErrIPNotSet
	}
	return &LocalNode{
		node: &livekit.Node{
			Id:      nodeID,
			Ip:      conf.RTC.NodeIP,
			NumCpus: uint32(runtime.NumCPU()),
			Region:  conf.Region,
			State:   livekit.NodeState_SERVING,
			Stats: &livekit.NodeStats{
				StartedAt: time.Now().Unix(),
				UpdatedAt: time.Now().Unix(),
			},
		},
	}, nil
}

func (l *LocalNode) Clone() *livekit.Node {
	l.lock.RLock()
	defer l.lock.RUnlock()

	return utils.CloneProto(l.node)
}

func (l *LocalNode) NodeID() livekit.NodeID {
	l.lock.RLock()
	defer l.lock.RUnlock()

	return livekit.NodeID(l.node.Id)
}

func (l *LocalNode) NodeType() livekit.NodeType {
	l.lock.RLock()
	defer l.lock.RUnlock()

	return l.node.Type
}

func (l *LocalNode) NodeIP() string {
	l.lock.RLock()
	defer l.lock.RUnlock()

	return l.node.Ip
}

func (l *LocalNode) Region() string {
	l.lock.RLock()
	defer l.lock.RUnlock()

	return l.node.Region
}

func (l *LocalNode) SetState(state livekit.NodeState) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.node.State = state
}

func (l *LocalNode) UpdateNodeStats() bool {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.prevStats == nil {
		l.prevStats = l.node.Stats
	}
	updated, computedAvg, err := prometheus.GetUpdatedNodeStats(l.node.Stats, l.prevStats)
	if err != nil {
		logger.Errorw("could not update node stats", err)
		return false
	}
	l.node.Stats = updated
	if computedAvg {
		l.prevStats = updated
	}
	return true
}

func (l *LocalNode) SecondsSinceNodeStatsUpdate() float64 {
	l.lock.RLock()
	defer l.lock.RUnlock()

	return time.Since(time.Unix(0, l.node.Stats.UpdatedAt)).Seconds()
}
