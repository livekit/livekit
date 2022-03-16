package selector

import (
	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"
)

// CPULoadSelector eliminates nodes that have CPU usage higher than CPULoadLimit
// then selects a node randomly from nodes that are not overloaded
type CPULoadSelector struct {
	CPULoadLimit float32
}

func (s *CPULoadSelector) filterNodes(nodes []*livekit.Node) ([]*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	nodesLowLoad := make([]*livekit.Node, 0)
	for _, node := range nodes {
		stats := node.Stats
		if stats.CpuLoad < s.CPULoadLimit {
			nodesLowLoad = append(nodesLowLoad, node)
		}
	}
	if len(nodesLowLoad) > 0 {
		nodes = nodesLowLoad
	}
	return nodes, nil
}

func (s *CPULoadSelector) SelectNode(nodes []*livekit.Node) (*livekit.Node, error) {
	nodes, err := s.filterNodes(nodes)
	if err != nil {
		return nil, err
	}

	idx := funk.RandomInt(0, len(nodes))
	return nodes[idx], nil
}
