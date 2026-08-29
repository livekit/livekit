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
	"github.com/livekit/protocol/livekit"
)

// CPULoadSelector eliminates nodes that have CPU usage higher than CPULoadLimit
// then selects a node from nodes that are not overloaded
type CPULoadSelector struct {
	CPULoadLimit float32
	SortBy       string
	Algorithm    string
}

func (s *CPULoadSelector) filterNodes(nodes []*livekit.Node) ([]*livekit.Node, error) {
	nodes, err := FilterNodesByCriteria(nodes, s.CPULoadLimit, func(node *livekit.Node) float32 {
		return node.Stats.CpuLoad
	})
	if err != nil {
		return nil, err
	}

	return nodes, nil
}

func (s *CPULoadSelector) SelectNode(nodes []*livekit.Node) (*livekit.Node, error) {
	nodes, err := s.filterNodes(nodes)
	if err != nil {
		return nil, err
	}

	return SelectSortedNode(nodes, s.SortBy, s.Algorithm)
}
