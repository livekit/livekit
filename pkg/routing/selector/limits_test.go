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
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/selector"
)

func newTestNodeWithStats(cpuLoad, sysload float32, bytesPerSec int64) *livekit.Node {
	return &livekit.Node{
		Id:    guid.New(utils.NodePrefix),
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			CpuLoad:         cpuLoad,
			LoadAvgLast1Min: sysload,
			Rates: []*livekit.NodeStatsRate{
				{
					BytesIn:  float32(bytesPerSec) / 2,
					BytesOut: float32(bytesPerSec) / 2,
				},
			},
		},
	}
}

func TestLimitsFilter(t *testing.T) {
	nodeA := newTestNodeWithStats(0.2, 0.2, 1000)
	nodeB := newTestNodeWithStats(0.5, 0.8, 5000)
	nodeC := newTestNodeWithStats(0.9, 0.4, 20000)

	allNodes := []*livekit.Node{nodeA, nodeB, nodeC}

	t.Run("no limits returns all available nodes", func(t *testing.T) {
		f := &selector.LimitsFilter{}
		nodes, err := f.Filter(allNodes)
		require.NoError(t, err)
		require.Equal(t, 3, len(nodes))
	})

	t.Run("filters by CPU load only", func(t *testing.T) {
		f := &selector.LimitsFilter{CPULoadLimit: 0.6}
		nodes, err := f.Filter(allNodes)
		require.NoError(t, err)
		require.Equal(t, []*livekit.Node{nodeA, nodeB}, nodes)
	})

	t.Run("filters by sysload only", func(t *testing.T) {
		f := &selector.LimitsFilter{SysloadLimit: 0.5}
		nodes, err := f.Filter(allNodes)
		require.NoError(t, err)
		require.Equal(t, []*livekit.Node{nodeA, nodeC}, nodes)
	})

	t.Run("filters by bandwidth/bytespersec only", func(t *testing.T) {
		f := &selector.LimitsFilter{BytesPerSecLimit: 10000}
		nodes, err := f.Filter(allNodes)
		require.NoError(t, err)
		require.Equal(t, []*livekit.Node{nodeA, nodeB}, nodes)
	})

	t.Run("filters by multiple metrics simultaneously (CPU and bandwidth)", func(t *testing.T) {
		// nodeA: CPU 0.2 (<0.6), BW 1000 (<3000) -> PASS
		// nodeB: CPU 0.5 (<0.6), BW 5000 (NOT <3000) -> FAIL
		// nodeC: CPU 0.9 (NOT <0.6), BW 20000 (NOT <3000) -> FAIL
		f := &selector.LimitsFilter{
			CPULoadLimit:     0.6,
			BytesPerSecLimit: 3000,
		}
		nodes, err := f.Filter(allNodes)
		require.NoError(t, err)
		require.Equal(t, []*livekit.Node{nodeA}, nodes)
	})

	t.Run("filters by all three metrics simultaneously", func(t *testing.T) {
		// nodeA: CPU 0.2 (<0.8), sys 0.2 (<0.5), BW 1000 (<6000) -> PASS
		// nodeB: CPU 0.5 (<0.8), sys 0.8 (NOT <0.5), BW 5000 (<6000) -> FAIL
		// nodeC: CPU 0.9 (NOT <0.8), sys 0.4 (<0.5), BW 20000 (NOT <6000) -> FAIL
		f := &selector.LimitsFilter{
			CPULoadLimit:     0.8,
			SysloadLimit:     0.5,
			BytesPerSecLimit: 6000,
		}
		nodes, err := f.Filter(allNodes)
		require.NoError(t, err)
		require.Equal(t, []*livekit.Node{nodeA}, nodes)
	})

	t.Run("gracefully falls back to all available nodes if all exceed limits", func(t *testing.T) {
		f := &selector.LimitsFilter{
			CPULoadLimit: 0.1, // none have CPU < 0.1
		}
		nodes, err := f.Filter(allNodes)
		require.NoError(t, err)
		require.Equal(t, 3, len(nodes), "should fall back to all available nodes when all are loaded")
	})

	t.Run("returns error when no nodes available", func(t *testing.T) {
		f := &selector.LimitsFilter{CPULoadLimit: 0.5}
		_, err := f.Filter(nil)
		require.ErrorIs(t, err, selector.ErrNoAvailableNodes)
	})
}

func TestLimitsSelector(t *testing.T) {
	nodeA := newTestNodeWithStats(0.2, 0.4, 2000)
	nodeB := newTestNodeWithStats(0.4, 0.2, 1000)
	nodeC := newTestNodeWithStats(0.9, 0.9, 50000)

	allNodes := []*livekit.Node{nodeA, nodeB, nodeC}

	t.Run("selects lowest sorted node among filtered candidates", func(t *testing.T) {
		sel := &selector.LimitsSelector{
			LimitsFilter: selector.LimitsFilter{
				CPULoadLimit: 0.5, // excludes nodeC
			},
			SortBy:    "bytespersec",
			Algorithm: "lowest",
		}
		// Between nodeA (2000 bps) and nodeB (1000 bps), nodeB has lower bandwidth
		chosen, err := sel.SelectNode(allNodes)
		require.NoError(t, err)
		require.Equal(t, nodeB, chosen)
	})

	t.Run("created via CreateNodeSelector with kind multi", func(t *testing.T) {
		conf := &config.Config{
			NodeSelector: config.NodeSelectorConfig{
				Kind:             "multi",
				CPULoadLimit:     0.5,
				BytesPerSecLimit: 5000,
				SortBy:           "cpuload",
				Algorithm:        "lowest",
			},
		}
		sel, err := selector.CreateNodeSelector(conf)
		require.NoError(t, err)

		chosen, err := sel.SelectNode(allNodes)
		require.NoError(t, err)
		require.Equal(t, nodeA, chosen)
	})
}

func TestRegionAwareSelector_MultiMetric(t *testing.T) {
	rc := []config.RegionConfig{
		{
			Name: "us-west",
			Lat:  37.640466,
			Lon:  -120.880262,
		},
		{
			Name: "us-east",
			Lat:  40.689143,
			Lon:  -74.044457,
		},
	}

	nodeWestHighCPU := newTestNodeWithStats(0.95, 0.2, 1000)
	nodeWestHighCPU.Region = "us-west"

	nodeWestLowCPU := newTestNodeWithStats(0.3, 0.2, 1000)
	nodeWestLowCPU.Region = "us-west"

	nodeEastLowCPU := newTestNodeWithStats(0.2, 0.2, 1000)
	nodeEastLowCPU.Region = "us-east"

	t.Run("combines regional preference with CPU load limit", func(t *testing.T) {
		s, err := selector.NewRegionAwareSelector("us-west", rc, "random", "lowest")
		require.NoError(t, err)
		s.CPULoadLimit = 0.8 // eliminates nodeWestHighCPU

		nodes := []*livekit.Node{nodeWestHighCPU, nodeWestLowCPU, nodeEastLowCPU}
		chosen, err := s.SelectNode(nodes)
		require.NoError(t, err)
		require.Equal(t, nodeWestLowCPU, chosen, "should pick low-CPU node in closest region")
	})

	t.Run("combines regional preference with bandwidth limit", func(t *testing.T) {
		nodeWestHeavyBW := newTestNodeWithStats(0.2, 0.2, 100_000_000)
		nodeWestHeavyBW.Region = "us-west"

		s, err := selector.NewRegionAwareSelector("us-west", rc, "random", "lowest")
		require.NoError(t, err)
		s.BytesPerSecLimit = 50_000_000 // eliminates nodeWestHeavyBW

		nodes := []*livekit.Node{nodeWestHeavyBW, nodeEastLowCPU}
		chosen, err := s.SelectNode(nodes)
		require.NoError(t, err)
		require.Equal(t, nodeEastLowCPU, chosen, "should fail over to east node when west exceeds bandwidth limit")
	})

	t.Run("supports custom NodeFilter injection", func(t *testing.T) {
		s, err := selector.NewRegionAwareSelector("us-west", rc, "random", "lowest")
		require.NoError(t, err)
		s.Filter = selector.NodeFilterFunc(func(nodes []*livekit.Node) ([]*livekit.Node, error) {
			// Custom filter: only allow east nodes
			var filtered []*livekit.Node
			for _, n := range nodes {
				if n.Region == "us-east" {
					filtered = append(filtered, n)
				}
			}
			return filtered, nil
		})

		nodes := []*livekit.Node{nodeWestLowCPU, nodeEastLowCPU}
		chosen, err := s.SelectNode(nodes)
		require.NoError(t, err)
		require.Equal(t, nodeEastLowCPU, chosen)
	})
}
