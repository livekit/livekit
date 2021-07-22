package routing

import (
	"errors"
	"strings"
	"time"

	livekit "github.com/livekit/livekit-server/proto"
	"github.com/thoas/go-funk"
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

func ParticipantKey(roomName, identity string) string {
	return roomName + "|" + identity
}

func parseParticipantKey(pkey string) (roomName string, identity string, err error) {
	parts := strings.Split(pkey, "|")
	if len(parts) != 2 {
		err = errors.New("invalid participant key")
		return
	}

	return parts[0], parts[1], nil
}
