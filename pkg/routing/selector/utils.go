package selector

import (
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/config"
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

// TODO: check remote node configured limit, instead of this node's config
func LimitsReached(limitConfig config.LimitConfig, nodeStats *livekit.NodeStats) bool {
	if nodeStats == nil {
		return false
	}

	if limitConfig.NumTracks > 0 && limitConfig.NumTracks <= nodeStats.NumTracksIn+nodeStats.NumTracksOut {
		return true
	}
	if limitConfig.BytesPerSec > 0 && limitConfig.BytesPerSec <= nodeStats.BytesInPerSec+nodeStats.BytesOutPerSec {
		return true
	}

	return false
}
