package routing

import (
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/thoas/go-funk"
)

type RandomSelector struct {
}

func (s *RandomSelector) SelectNode(nodes []*livekit.Node, room *livekit.Room) (*livekit.Node, error) {
	nodes = GetAvailableNodes(nodes)
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	idx := funk.RandomInt(0, len(nodes))
	return nodes[idx], nil
}
