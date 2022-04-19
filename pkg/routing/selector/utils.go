package selector

import (
	"sort"
	"time"

	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
)

const AvailableSeconds = 5

// checks if a node has been updated recently to be considered for selection
func IsAvailable(node *livekit.Node) bool {
	if node.Stats == nil {
		// available till stats are available
		return true
	}

	delta := time.Now().Unix() - node.Stats.UpdatedAt
	return int(delta) < AvailableSeconds
}

func GetAvailableNodes(nodes []*livekit.Node) []*livekit.Node {
	return funk.Filter(nodes, func(node *livekit.Node) bool {
		return IsAvailable(node) && node.State == livekit.NodeState_SERVING
	}).([]*livekit.Node)
}

func GetNodeSysload(node *livekit.Node) float32 {
	stats := node.Stats
	numCpus := stats.NumCpus
	if numCpus == 0 {
		numCpus = 1
	}
	return stats.LoadAvgLast1Min / float32(numCpus)
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

func SelectSortedNode(nodes []*livekit.Node, sortBy string) (*livekit.Node, error) {
	if sortBy == "" {
		return nil, ErrSortByNotSet
	}

	// Return a node based on what it should be sorted by for priority
	switch sortBy {
	case "random":
		idx := funk.RandomInt(0, len(nodes))
		return nodes[idx], nil
	case "sysload":
		sort.Slice(nodes, func(i, j int) bool {
			return GetNodeSysload(nodes[i]) < GetNodeSysload(nodes[j])
		})
		return nodes[0], nil
	case "cpuload":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.CpuLoad < nodes[j].Stats.CpuLoad
		})
		return nodes[0], nil
	case "rooms":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.NumRooms < nodes[j].Stats.NumRooms
		})
		return nodes[0], nil
	case "clients":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.NumClients < nodes[j].Stats.NumClients
		})
		return nodes[0], nil
	case "tracks":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.NumTracksIn+nodes[i].Stats.NumTracksOut < nodes[j].Stats.NumTracksIn+nodes[j].Stats.NumTracksOut
		})
		return nodes[0], nil
	case "bytespersec":
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Stats.BytesInPerSec+nodes[i].Stats.BytesOutPerSec < nodes[j].Stats.BytesInPerSec+nodes[j].Stats.BytesOutPerSec
		})
		return nodes[0], nil
	default:
		return nil, ErrSortByUnknown
	}
}
