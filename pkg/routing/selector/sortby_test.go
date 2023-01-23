package selector_test

import (
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

func SortByTest(t *testing.T, sortBy string) {
	f := selector.SystemLoadSelector{SysloadLimit: loadLimit}
	sel := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{&f}}
	nodes := []*livekit.Node{nodeLoadLow, nodeLoadMedium, nodeLoadHigh}

	for i := 0; i < 5; i++ {
		node, err := sel.SelectNode(nodes, selector.AssignMeeting)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node for SortBy:", sortBy)
		}
	}
}

func TestSortByErrors(t *testing.T) {
	f := selector.SystemLoadSelector{}
	sel := selector.NodeSelectorBase{Selectors: []selector.NodeFilter{&f}}
	nodes := []*livekit.Node{nodeLoadLow, nodeLoadMedium, nodeLoadHigh}

	// Test unset sort by option error
	_, err := sel.SelectNode(nodes, selector.AssignMeeting)
	if err != selector.ErrSortByNotSet {
		t.Error("shouldn't allow empty sortBy")
	}

	// Test unknown sort by option error
	sel.SortBy = "testFail"
	_, err = sel.SelectNode(nodes, selector.AssignMeeting)
	if err != selector.ErrSortByUnknown {
		t.Error("shouldn't allow unknown sortBy")
	}
}

func TestSortBy(t *testing.T) {
	sortByTests := []string{"sysload", "cpuload", "rooms", "clients", "tracks", "bytespersec"}

	for _, sortBy := range sortByTests {
		SortByTest(t, sortBy)
	}
}
