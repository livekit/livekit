package routing_test

import (
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/routing"
	livekit "github.com/livekit/livekit-server/proto"
)

var (
	nodeLoadLow = &livekit.Node{
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			LoadAvgLast1Min: 0.0,
		},
	}

	nodeLoadHigh = &livekit.Node{
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			LoadAvgLast1Min: 2.0,
		},
	}
)

func TestSystemLoadSelector_SelectNode(t *testing.T) {
	selector := routing.SystemLoadSelector{SysloadLimit: 1.0}

	nodes := []*livekit.Node{}
	err := selector.SelectNode(nodes, nil)
	require.Error(t, err, "should error no available nodes")

	// Select a node with high load when no nodes with low load are available
	nodes = []*livekit.Node{nodeLoadHigh}
	if _, err := selector.SelectNode(nodes, nil); err != nil {
		t.Error(err)
	}

	// Select a node with low load when available
	nodes = []*livekit.Node{nodeLoadLow, nodeLoadHigh}
	for i := 0; i < 5; i++ {
		node, err := selector.SelectNode(nodes, nil)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node")
		}
	}
}
