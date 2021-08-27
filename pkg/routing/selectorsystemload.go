package routing

import (
	livekit "github.com/livekit/protocol/proto"
	"github.com/thoas/go-funk"
)

type SystemLoadSelector struct {
	SysloadLimit float32
}

func (s *SystemLoadSelector) SelectNode(nodes []*livekit.Node, room *livekit.Room) (*livekit.Node, error) {
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

	idx := funk.RandomInt(0, len(nodes))
	return nodes[idx], nil
}
