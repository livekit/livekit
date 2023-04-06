package selector

import (
	"github.com/livekit/protocol/livekit"
)

// AnySelector selects any available node with no limitations
type AnySelector struct {
}

func (s *AnySelector) FilterNodes(nodes []*livekit.Node, selectionType NodeSelection) ([]*livekit.Node, error) {
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}
	return nodes, nil
}
