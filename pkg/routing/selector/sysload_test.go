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

package selector_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

var (
	nodeLoadLow = &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			CpuLoad:         0.1,
			LoadAvgLast1Min: 0.0,
			NumRooms:        1,
			NumClients:      2,
			NumTracksIn:     4,
			NumTracksOut:    8,
			Rates: []*livekit.NodeStatsRate{
				{
					BytesIn:  1000,
					BytesOut: 2000,
				},
			},
		},
	}

	nodeLoadMedium = &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			CpuLoad:         0.5,
			LoadAvgLast1Min: 0.5,
			NumRooms:        5,
			NumClients:      10,
			NumTracksIn:     20,
			NumTracksOut:    200,
			Rates: []*livekit.NodeStatsRate{
				{
					BytesIn:  5000,
					BytesOut: 10000,
				},
			},
		},
	}

	nodeLoadHigh = &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			CpuLoad:         0.99,
			LoadAvgLast1Min: 2.0,
			NumRooms:        10,
			NumClients:      20,
			NumTracksIn:     40,
			NumTracksOut:    800,
			Rates: []*livekit.NodeStatsRate{
				{
					BytesIn:  10000,
					BytesOut: 40000,
				},
			},
		},
	}
)

func TestSystemLoadSelector_SelectNode(t *testing.T) {
	sel := selector.SystemLoadSelector{SysloadLimit: 1.0, SortBy: "random", Algorithm: "lowest"}

	var nodes []*livekit.Node
	_, err := sel.SelectNode(nodes)
	require.Error(t, err, "should error no available nodes")

	// Select a node with high load when no nodes with low load are available
	nodes = []*livekit.Node{nodeLoadHigh}
	if _, err := sel.SelectNode(nodes); err != nil {
		t.Error(err)
	}

	// Select a node with low load when available
	nodes = []*livekit.Node{nodeLoadLow, nodeLoadHigh}
	for i := 0; i < 5; i++ {
		node, err := sel.SelectNode(nodes)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node")
		}
	}
}
