package selector_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

var (
	nodeLoadLow = &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			CpuLoad:         0.1,
			LoadAvgLast1Min: 0.0,
			NumRooms:        1,
			NumClients:      2,
			NumTracksIn:     4,
			NumTracksOut:    8,
			BytesInPerSec:   1000,
			BytesOutPerSec:  2000,
		},
	}

	nodeLoadMedium = &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			CpuLoad:         0.5,
			LoadAvgLast1Min: 0.5,
			NumRooms:        5,
			NumClients:      10,
			NumTracksIn:     20,
			NumTracksOut:    200,
			BytesInPerSec:   5000,
			BytesOutPerSec:  10000,
		},
	}

	nodeLoadHigh = &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			CpuLoad:         0.99,
			LoadAvgLast1Min: 2.0,
			NumRooms:        10,
			NumClients:      20,
			NumTracksIn:     40,
			NumTracksOut:    800,
			BytesInPerSec:   10000,
			BytesOutPerSec:  40000,
		},
	}
)

func TestSystemLoadSelector_SelectNode(t *testing.T) {
	sel := selector.SystemLoadSelector{SysloadLimit: 1.0, SortBy: "random"}

	var nodes []*livekit.Node
	_, err := sel.SelectNode(nodes)
	require.Error(t, err, "should error no available nodes")

	// Select a node with high load when no nodes with low load are available
	nodes = []*livekit.Node{nodeLoadHigh}
	if _, err := sel.SelectNode(nodes); err != nil {
		t.Error(err)
	}

	// Select a node with low load when available
	nodes = []*livekit.Node{nodeLoadLow, nodeLoadHigh}
	for i := 0; i < 5; i++ {
		node, err := sel.SelectNode(nodes)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node")
		}
	}
}
