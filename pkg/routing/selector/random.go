package selector

import (
	"github.com/livekit/protocol/livekit"
)

// RandomSelector selects an available node at random
type RandomSelector struct {
	SortBy string
}

func (s *RandomSelector) SelectNode(nodes []*livekit.Node) (*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	return SelectSortedNode(nodes, s.SortBy)
}
