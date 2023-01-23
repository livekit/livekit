package selector_test

import (
	"github.com/livekit/protocol/utils"
	"github.com/stretchr/testify/require"
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/protocol/livekit"
)

func TestSimpleRegionAwareRouting(t *testing.T) {
	rc := []config.RegionConfig{
		{
			Name: regionWest,
			Lat:  37.64046607830567,
			Lon:  -120.88026233189062,
		},
		{
			Name: regionEast,
			Lat:  40.68914362140307,
			Lon:  -74.04445748616385,
		},
		{
			Name: regionSeattle,
			Lat:  47.620426730945454,
			Lon:  -122.34938468973702,
		},
	}

	t.Run("works without region config", func(t *testing.T) {
		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion("", true),
		}
		f, err := selector.NewSimpleRegionAwareSelector(regionEast, nil)
		require.NoError(t, err)

		s := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{f}}
		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.NotNil(t, node)
	})

	t.Run("fails with region config with no available nodes", func(t *testing.T) {
		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion("", false),
		}
		f, err := selector.NewSimpleRegionAwareSelector(regionEast, nil)
		require.NoError(t, err)

		s := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{f}}
		_, err = s.SelectNode(nodes, selector.AssignMeeting)
		require.Error(t, err, selector.ErrNoAvailableNodes)
	})

	t.Run("picks available nodes in same region", func(t *testing.T) {
		expectedNode := newTestNodeForSimpleRegion(regionEast, true)

		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion(regionSeattle, true),
			newTestNodeForSimpleRegion(regionWest, true),
			expectedNode,
			newTestNodeForSimpleRegion(regionEast, false),
		}
		f, err := selector.NewSimpleRegionAwareSelector(regionEast, rc)
		require.NoError(t, err)

		s := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{f}}
		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.Equal(t, expectedNode, node)
	})

	t.Run("picks available nodes in same region when current node is first in the list", func(t *testing.T) {
		expectedNode := newTestNodeForSimpleRegion(regionEast, true)
		nodes := []*livekit.Node{
			expectedNode,
			newTestNodeForSimpleRegion(regionSeattle, true),
			newTestNodeForSimpleRegion(regionWest, true),
			newTestNodeForSimpleRegion(regionEast, false),
		}
		f, err := selector.NewSimpleRegionAwareSelector(regionEast, rc)
		require.NoError(t, err)

		s := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{f}}
		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.Equal(t, expectedNode, node)
	})

	t.Run("picks closest node in a diff region", func(t *testing.T) {
		expectedNode := newTestNodeForSimpleRegion(regionWest, true)
		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion(regionSeattle, false),
			expectedNode,
			newTestNodeForSimpleRegion(regionEast, true),
		}
		f, err := selector.NewSimpleRegionAwareSelector(regionSeattle, rc)
		require.NoError(t, err)

		s := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{f}}
		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.Equal(t, expectedNode, node)
	})

	t.Run("handles multiple nodes in same region", func(t *testing.T) {
		expectedNode := newTestNodeForSimpleRegion(regionWest, true)
		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion(regionSeattle, false),
			newTestNodeForSimpleRegion(regionEast, true),
			newTestNodeForSimpleRegion(regionEast, true),
			expectedNode,
			expectedNode,
		}
		f, err := selector.NewSimpleRegionAwareSelector(regionSeattle, rc)
		require.NoError(t, err)

		s := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{f}}
		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.Equal(t, expectedNode, node)
	})

	t.Run("functions when current region is full", func(t *testing.T) {
		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion(regionWest, true),
		}
		f, err := selector.NewSimpleRegionAwareSelector(regionEast, rc)
		require.NoError(t, err)

		s := selector.NodeSelectorBase{SortBy: "random", Selectors: []selector.NodeFilter{f}}
		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.NotNil(t, node)
	})
}

func newTestNodeForSimpleRegion(region string, available bool) *livekit.Node {
	load := float32(0.4)
	state := livekit.NodeState_SERVING
	if !available {
		load = 1.0
		state = livekit.NodeState_SHUTTING_DOWN
	}
	return &livekit.Node{
		Id:     utils.NewGuid(utils.NodePrefix),
		Region: region,
		State:  state,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix(),
			NumCpus:         1,
			LoadAvgLast1Min: load,
		},
	}
}
