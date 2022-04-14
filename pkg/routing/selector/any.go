package selector

import (
	"github.com/livekit/protocol/livekit"
)

// AnySelector selects any available node with no limitations
type AnySelector struct {
	SortBy string
}

func (s *AnySelector) SelectNode(nodes []*livekit.Node) (*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	return SelectSortedNode(nodes, s.SortBy)
}
