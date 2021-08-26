package routing

import (
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/thoas/go-funk"
)

type SystemLoadSelector struct {
	LoadLevelHigh float32
}

func (s *SystemLoadSelector) SelectNode(nodes []*livekit.Node, room *livekit.Room) (*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	nodesLowLoad := []*livekit.Node{}
	for _, node := range nodes {
		if node.Stats.LoadAvgLast1Min/float32(node.Stats.NumCpus) < s.LoadLevelHigh {
			nodesLowLoad = append(nodesLowLoad, node)
		}
	}
	if len(nodesLowLoad) > 0 {
		nodes = nodesLowLoad
	}

	idx := funk.RandomInt(0, len(nodes))
	return nodes[idx], nil
}
