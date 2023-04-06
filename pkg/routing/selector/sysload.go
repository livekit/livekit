package selector

import (
	"github.com/livekit/protocol/livekit"
)

// SystemLoadSelector eliminates nodes that surpass has a per-cpu node higher than SysloadLimit
// then selects a node from nodes that are not overloaded
type SystemLoadSelector struct {
	SysloadLimit     float32
	HardSysloadLimit float32
}

func (s *SystemLoadSelector) FilterNodes(nodes []*livekit.Node, selectionType NodeSelection) ([]*livekit.Node, error) {
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}
	nodesLowLoad := make([]*livekit.Node, 0)
	nodesHighLoad := make([]*livekit.Node, 0)
	for _, node := range nodes {
		nodeSysLoad := GetNodeSysload(node)
		if nodeSysLoad < s.SysloadLimit {
			nodesLowLoad = append(nodesLowLoad, node)
		} else if s.HardSysloadLimit != 0.0 {
			if nodeSysLoad < s.HardSysloadLimit {
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
