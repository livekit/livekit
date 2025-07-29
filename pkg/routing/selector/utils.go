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

package selector

import (
	"math/rand/v2"
	"sort"
	"time"

	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
)

const AvailableSeconds = 5

// checks if a node has been updated recently to be considered for selection
func IsAvailable(node *livekit.Node) bool {
	if node.Stats == nil {
		// available till stats are available
		return true
	}

	delta := time.Now().Unix() - node.Stats.UpdatedAt
	return int(delta) < AvailableSeconds
}

func GetAvailableNodes(nodes []*livekit.Node) []*livekit.Node {
	return funk.Filter(nodes, func(node *livekit.Node) bool {
		return IsAvailable(node) && node.State == livekit.NodeState_SERVING
	}).([]*livekit.Node)
}

func GetNodeSysload(node *livekit.Node) float32 {
	stats := node.Stats
	numCpus := stats.NumCpus
	if numCpus == 0 {
		numCpus = 1
	}
	return stats.LoadAvgLast1Min / float32(numCpus)
}

// TODO: check remote node configured limit, instead of this node's config
func LimitsReached(limitConfig config.LimitConfig, nodeStats *livekit.NodeStats) bool {
	if nodeStats == nil {
		return false
	}

	if limitConfig.NumTracks > 0 && limitConfig.NumTracks <= nodeStats.NumTracksIn+nodeStats.NumTracksOut {
		return true
	}

	rate := &livekit.NodeStatsRate{}
	if len(nodeStats.Rates) > 0 {
		rate = nodeStats.Rates[0]
	}
	if limitConfig.BytesPerSec > 0 && limitConfig.BytesPerSec <= rate.BytesIn+rate.BytesOut {
		return true
	}

	return false
}

func SelectSortedNode(nodes []*livekit.Node, sortBy string, algorithm string) (*livekit.Node, error) {
	if sortBy == "" {
		return nil, ErrSortByNotSet
	}
	if algorithm == "" {
		return nil, ErrAlgorithmNotSet
	}

	switch algorithm {
	case "lowest": // examine all nodes and select the lowest based on sort criteria
		return selectLowestSortedNode(nodes, sortBy)
	case "twochoice": // randomly select two nodes and return the lowest based on sort criteria "Power of Two Random Choices"
		return selectTwoChoiceSortedNode(nodes, sortBy)
	default:
		return nil, ErrAlgorithmUnknown
	}
}

func selectTwoChoiceSortedNode(nodes []*livekit.Node, sortBy string) (*livekit.Node, error) {
	if len(nodes) <= 2 {
		return selectLowestSortedNode(nodes, sortBy)
	}

	// randomly select two nodes
	node1, node2, err := selectTwoRandomNodes(nodes)
	if err != nil {
		return nil, err
	}

	// compare the two nodes based on the sort criteria
	if node1 == nil || node2 == nil {
		return nil, ErrNoAvailableNodes
	}

	selectedNode, err := selectLowestSortedNode([]*livekit.Node{node1, node2}, sortBy)
	if err != nil {
		return nil, err
	}

	return selectedNode, nil
}

func selectLowestSortedNode(nodes []*livekit.Node, sortBy string) (*livekit.Node, error) {
	// Return a node based on what it should be sorted by for priority
	switch sortBy {
	case "random":
		idx := funk.RandomInt(0, len(nodes))
		return nodes[idx], nil
	case "sysload":
		sort.Slice(nodes, func(i, j int) bool {
			return GetNodeSysload(nodes[i]) < GetNodeSysload(nodes[j])
		})
		return nodes[0], nil
	case "cpuload":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.CpuLoad < nodes[j].Stats.CpuLoad
		})
		return nodes[0], nil
	case "rooms":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.NumRooms < nodes[j].Stats.NumRooms
		})
		return nodes[0], nil
	case "clients":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.NumClients < nodes[j].Stats.NumClients
		})
		return nodes[0], nil
	case "tracks":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.NumTracksIn+nodes[i].Stats.NumTracksOut < nodes[j].Stats.NumTracksIn+nodes[j].Stats.NumTracksOut
		})
		return nodes[0], nil
	case "bytespersec":
		sort.Slice(nodes, func(i, j int) bool {
			ratei := &livekit.NodeStatsRate{}
			if len(nodes[i].Stats.Rates) > 0 {
				ratei = nodes[i].Stats.Rates[0]
			}

			ratej := &livekit.NodeStatsRate{}
			if len(nodes[j].Stats.Rates) > 0 {
				ratej = nodes[j].Stats.Rates[0]
			}
			return ratei.BytesIn+ratei.BytesOut < ratej.BytesIn+ratej.BytesOut
		})
		return nodes[0], nil
	default:
		return nil, ErrSortByUnknown
	}
}

func selectTwoRandomNodes(nodes []*livekit.Node) (*livekit.Node, *livekit.Node, error) {
	if len(nodes) < 2 {
		return nil, nil, ErrNoAvailableNodes
	}

	shuffledIndices := rand.Perm(len(nodes))

	return nodes[shuffledIndices[0]], nodes[shuffledIndices[1]], nil
}
