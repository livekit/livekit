package selector_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

func TestCPULoadSelector_SelectNode(t *testing.T) {
	softFil := selector.CPULoadSelector{CPULoadLimit: 0.8}
	softSel := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{&softFil}}

	var nodes []*livekit.Node
	_, err := softSel.SelectNode(nodes, selector.AssignMeeting)
	require.Error(t, err, "should error no available nodes")

	// Select a node with high load when no nodes with low load are available with soflimit
	nodes = []*livekit.Node{nodeLoadHigh}
	if _, err := softSel.SelectNode(nodes, selector.AssignMeeting); err != nil {
		t.Error(err)
	}

	// Select a node with low load when available
	nodes = []*livekit.Node{nodeLoadLow, nodeLoadHigh}
	for i := 0; i < 5; i++ {
		node, err := softSel.SelectNode(nodes, selector.AssignMeeting)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node")
		}
	}

	nodes = []*livekit.Node{nodeLoadHigh}
	hardFil := selector.CPULoadSelector{CPULoadLimit: 0.8, HardCPULoadLimit: 0.8}
	hardSel := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{&hardFil}}
	// No node should be selected  when no nodes with low load are available with hardlimit
	_, err = hardSel.SelectNode(nodes, selector.AssignMeeting)
	require.Error(t, err, "should error no available nodes")

}
