package routing

import (
	"time"

	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/proto/livekit"
)

// checks if a node has been updated recently to be considered for selection
func IsAvailable(node *livekit.Node) bool {
	delta := time.Now().Unix() - node.Stats.UpdatedAt
	limit := statsUpdateInterval.Seconds() * 2
	return float64(delta) < limit
}

func GetAvailableNodes(nodes []*livekit.Node) []*livekit.Node {
	return funk.Filter(nodes, func(node *livekit.Node) bool {
		return IsAvailable(node)
	}).([]*livekit.Node)
}

func participantKey(roomName, identity string) string {
	return roomName + "|" + identity
}
