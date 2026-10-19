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

// SystemLoadSelector eliminates nodes that surpass has a per-cpu node higher than SysloadLimit
// then selects a node from nodes that are not overloaded
type SystemLoadSelector struct {
	SysloadLimit float32
	SortBy       string
	Algorithm    string
}

func (s *SystemLoadSelector) filterNodes(nodes []*livekit.Node) ([]*livekit.Node, error) {
	nodes, err := FilterNodesByCriteria(nodes, s.SysloadLimit, GetNodeSysload)
	if err != nil {
		return nil, err
	}

	return nodes, nil
}

func (s *SystemLoadSelector) SelectNode(nodes []*livekit.Node) (*livekit.Node, error) {
	nodes, err := s.filterNodes(nodes)
	if err != nil {
		return nil, err
	}

	return SelectSortedNode(nodes, s.SortBy, s.Algorithm)
}
