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

// NodeFilter filters a list of candidate nodes based on custom conditions
type NodeFilter interface {
	Filter(nodes []*livekit.Node) ([]*livekit.Node, error)
}

// NodeFilterFunc is an adapter allowing the use of an ordinary function as a NodeFilter
type NodeFilterFunc func(nodes []*livekit.Node) ([]*livekit.Node, error)

func (f NodeFilterFunc) Filter(nodes []*livekit.Node) ([]*livekit.Node, error) {
	return f(nodes)
}

// LimitsFilter filters nodes that exceed one or more resource thresholds (Sysload, CPULoad, BytesPerSec)
type LimitsFilter struct {
	SysloadLimit     float32
	CPULoadLimit     float32
	BytesPerSecLimit float32
}

func NewLimitsFilter(sysloadLimit, cpuLoadLimit, bytesPerSecLimit float32) *LimitsFilter {
	return &LimitsFilter{
		SysloadLimit:     sysloadLimit,
		CPULoadLimit:     cpuLoadLimit,
		BytesPerSecLimit: bytesPerSecLimit,
	}
}

func (f *LimitsFilter) HasLimits() bool {
	return f.SysloadLimit > 0 || f.CPULoadLimit > 0 || f.BytesPerSecLimit > 0
}

func (f *LimitsFilter) Filter(nodes []*livekit.Node) ([]*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	if !f.HasLimits() {
		return nodes, nil
	}

	filteredNodes := make([]*livekit.Node, 0, len(nodes))
	for _, node := range nodes {
		if f.isNodeUnderLimits(node) {
			filteredNodes = append(filteredNodes, node)
		}
	}

	// Graceful fallback to all available nodes if every node exceeds limits
	if len(filteredNodes) > 0 {
		nodes = filteredNodes
	}
	return nodes, nil
}

func (f *LimitsFilter) isNodeUnderLimits(node *livekit.Node) bool {
	if node.Stats == nil {
		return true
	}
	if f.SysloadLimit > 0 && GetNodeSysload(node) >= f.SysloadLimit {
		return false
	}
	if f.CPULoadLimit > 0 && node.Stats.CpuLoad >= f.CPULoadLimit {
		return false
	}
	if f.BytesPerSecLimit > 0 && GetNodeBytesPerSec(node) >= f.BytesPerSecLimit {
		return false
	}
	return true
}

// LimitsSelector filters nodes across multiple resource limits (sysload, cpu load, bandwidth)
// and then chooses a node using the configured sorting algorithm
type LimitsSelector struct {
	LimitsFilter
	SortBy    string
	Algorithm string
}

func (s *LimitsSelector) SelectNode(nodes []*livekit.Node) (*livekit.Node, error) {
	nodes, err := s.Filter(nodes)
	if err != nil {
		return nil, err
	}

	return SelectSortedNode(nodes, s.SortBy, s.Algorithm)
}
