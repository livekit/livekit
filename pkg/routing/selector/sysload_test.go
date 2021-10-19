package selector_test

import (
	"testing"
	"time"

	livekit "github.com/livekit/protocol/proto"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

var (
	nodeLoadLow = &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			LoadAvgLast1Min: 0.0,
		},
	}

	nodeLoadHigh = &livekit.Node{
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			LoadAvgLast1Min: 2.0,
		},
	}
)

func TestSystemLoadSelector_SelectNode(t *testing.T) {
	selector := selector.SystemLoadSelector{SysloadLimit: 1.0}

	nodes := []*livekit.Node{}
	_, err := selector.SelectNode(nodes)
	require.Error(t, err, "should error no available nodes")

	// Select a node with high load when no nodes with low load are available
	nodes = []*livekit.Node{nodeLoadHigh}
	if _, err := selector.SelectNode(nodes); err != nil {
		t.Error(err)
	}

	// Select a node with low load when available
	nodes = []*livekit.Node{nodeLoadLow, nodeLoadHigh}
	for i := 0; i < 5; i++ {
		node, err := selector.SelectNode(nodes)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node")
		}
	}
}
