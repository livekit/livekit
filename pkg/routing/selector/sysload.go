package selector

import (
	livekit "github.com/livekit/protocol/proto"
	"github.com/thoas/go-funk"
)

// SystemLoadSelector eliminates nodes that surpass has a per-cpu node higher than SysloadLimit
// then selects a node randomly from nodes that are not overloaded
type SystemLoadSelector struct {
	SysloadLimit float32
}

func (s *SystemLoadSelector) filterNodes(nodes []*livekit.Node) ([]*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	nodesLowLoad := make([]*livekit.Node, 0)
	for _, node := range nodes {
		numCpus := node.Stats.NumCpus
		if numCpus == 0 {
			numCpus = 1
		}
		if node.Stats.LoadAvgLast1Min/float32(numCpus) < s.SysloadLimit {
			nodesLowLoad = append(nodesLowLoad, node)
		}
	}
	if len(nodesLowLoad) > 0 {
		nodes = nodesLowLoad
	}
	return nodes, nil
}

func (s *SystemLoadSelector) SelectNode(nodes []*livekit.Node, room *livekit.Room) (*livekit.Node, error) {
	nodes, err := s.filterNodes(nodes)
	if err != nil {
		return nil, err
	}

	idx := funk.RandomInt(0, len(nodes))
	return nodes[idx], nil
}
