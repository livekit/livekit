package selector

import (
	"github.com/livekit/protocol/livekit"
)

// CPULoadSelector eliminates nodes that have CPU usage higher than CPULoadLimit
// then selects a node from nodes that are not overloaded
type CPULoadSelector struct {
	CPULoadLimit     float32
	HardCPULoadLimit float32
}

func (s *CPULoadSelector) FilterNodes(nodes []*livekit.Node, selectionType NodeSelection) ([]*livekit.Node, error) {

	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	nodesLowLoad := make([]*livekit.Node, 0)
	nodesHighLoad := make([]*livekit.Node, 0)
	for _, node := range nodes {
		stats := node.Stats
		if stats.CpuLoad < s.CPULoadLimit {
			nodesLowLoad = append(nodesLowLoad, node)
		} else if s.HardCPULoadLimit != 0.0 {
			if stats.CpuLoad < s.HardCPULoadLimit {
				nodesHighLoad = append(nodesHighLoad, node)
			}
		} else {
			nodesHighLoad = append(nodesHighLoad, node)
		}
	}

	if len(nodesLowLoad) > 0 {
		nodes = nodesLowLoad
	} else {
		nodes = nodesHighLoad
	}
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	return nodes, nil
}
