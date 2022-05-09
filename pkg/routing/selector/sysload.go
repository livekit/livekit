package selector

import (
	"github.com/livekit/protocol/livekit"
)

// SystemLoadSelector eliminates nodes that surpass has a per-cpu node higher than SysloadLimit
// then selects a node from nodes that are not overloaded
type SystemLoadSelector struct {
	SysloadLimit float32
	SortBy       string
}

func (s *SystemLoadSelector) filterNodes(nodes []*livekit.Node) ([]*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	nodesLowLoad := make([]*livekit.Node, 0)
	for _, node := range nodes {
		if GetNodeSysload(node) < s.SysloadLimit {
			nodesLowLoad = append(nodesLowLoad, node)
		}
	}
	if len(nodesLowLoad) > 0 {
		nodes = nodesLowLoad
	}
	return nodes, nil
}

func (s *SystemLoadSelector) SelectNode(nodes []*livekit.Node) (*livekit.Node, error) {
	nodes, err := s.filterNodes(nodes)
	if err != nil {
		return nil, err
	}

	return SelectSortedNode(nodes, s.SortBy)
}
