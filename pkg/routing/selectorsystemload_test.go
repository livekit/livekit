package routing_test

import (
	"testing"
	"time"

	livekit "github.com/livekit/protocol/proto"
	"github.com/stretchr/testify/require"
	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/routing"
)

var (
	nodeLoadLow = &livekit.Node{
		Id: funk.RandomString(8),
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			LoadAvgLast1Min: 0.0,
		},
	}

	nodeLoadHigh = &livekit.Node{
		Id: funk.RandomString(8),
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			LoadAvgLast1Min: 2.0,
		},
	}

	preferredNodeLoadLow = &livekit.Node{
		Id: funk.RandomString(8),
		State: livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			LoadAvgLast1Min: 0.0,
		},
	}

	preferredNodeLoadHigh = &livekit.Node{
		Id: funk.RandomString(8),
		State: livekit.NodeState_SERVING,
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
	_, err := selector.SelectNode(nodes, nil, nil)
	require.Error(t, err, "should error no available nodes")

	// Select a node with high load when no nodes with low load are available
	nodes = []*livekit.Node{nodeLoadHigh}
	if _, err := selector.SelectNode(nodes, nil, nil); err != nil {
		t.Error(err)
	}

	// Select a node with low load when available
	nodes = []*livekit.Node{nodeLoadLow, nodeLoadHigh}
	for i := 0; i < 5; i++ {
		node, err := selector.SelectNode(nodes, nil, nil)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node")
		}
	}

	// Select a node while preferring a low load node
	nodes = []*livekit.Node{nodeLoadLow, nodeLoadHigh, preferredNodeLoadLow}
	for i := 0; i < 5; i++ {
		node, err := selector.SelectNode(nodes, nil, preferredNodeLoadLow)
		if err != nil {
			t.Error(err)
		}
		if node != preferredNodeLoadLow {
			t.Error("selected the wrong node")
		}
	}

	// Select a node while preferring a high load node
	nodes = []*livekit.Node{nodeLoadLow, nodeLoadHigh, preferredNodeLoadHigh}
	for i := 0; i < 5; i++ {
		node, err := selector.SelectNode(nodes, nil, preferredNodeLoadHigh)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node")
		}
	}
}
