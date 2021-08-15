package routing

import (
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/thoas/go-funk"
)

type RandomSelector struct {
}

func (s *RandomSelector) SelectNode(nodes []*livekit.Node, room *livekit.Room, preferredNode *livekit.Node) (*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	if preferredNode != nil && funk.Contains(nodes, func(node *livekit.Node) bool {
		return node.Id == preferredNode.Id
	}) {
		logger.Debugw("selecting preferred node")
		return preferredNode, nil
	} else {
		if preferredNode != nil {
			logger.Warnw("preferred node not available", ErrPrefNodeNotAvailable)
		}
		idx := funk.RandomInt(0, len(nodes))
		return nodes[idx], nil
	}
}
