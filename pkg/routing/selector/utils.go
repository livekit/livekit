package selector

import (
	"time"

	livekit "github.com/livekit/protocol/proto"
	"github.com/thoas/go-funk"
)

const AvailableSeconds = 5

// checks if a node has been updated recently to be considered for selection
func IsAvailable(node *livekit.Node) bool {
	delta := time.Now().Unix() - node.Stats.UpdatedAt
	return int(delta) < AvailableSeconds
}

func GetAvailableNodes(nodes []*livekit.Node) []*livekit.Node {
	return funk.Filter(nodes, func(node *livekit.Node) bool {
		return IsAvailable(node) && node.State == livekit.NodeState_SERVING
	}).([]*livekit.Node)
}
