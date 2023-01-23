package selector_test

import (
	"github.com/stretchr/testify/require"
	"testing"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/protocol/livekit"
)

func TestSysLoadSimpleRegionAwareRouting(t *testing.T) {
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
		s := getCompositeNodeSelector(t, regionEast, nil, 0.0)

		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.NotNil(t, node)
	})

	t.Run("picks no node without region config with hard limit", func(t *testing.T) {
		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion("", false),
		}
		s := getCompositeNodeSelector(t, regionEast, nil, loadLimit)

		_, err := s.SelectNode(nodes, selector.AssignMeeting)
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
		s := getCompositeNodeSelector(t, regionEast, rc, 0.0)

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
		s := getCompositeNodeSelector(t, regionEast, rc, 0.0)

		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.Equal(t, expectedNode, node)
	})

	t.Run("picks closest node in a diff region", func(t *testing.T) {
		expectedNode := newTestNodeInRegion(regionWest, true)
		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion(regionSeattle, false),
			expectedNode,
			newTestNodeForSimpleRegion(regionEast, true),
		}
		s := getCompositeNodeSelector(t, regionSeattle, rc, 0.0)

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
		s := getCompositeNodeSelector(t, regionSeattle, rc, 0.0)

		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.Equal(t, expectedNode, node)
	})

	t.Run("functions when current region is full", func(t *testing.T) {
		nodes := []*livekit.Node{
			newTestNodeForSimpleRegion(regionWest, true),
		}
		s := getCompositeNodeSelector(t, regionEast, rc, 0.0)

		node, err := s.SelectNode(nodes, selector.AssignMeeting)
		require.NoError(t, err)
		require.NotNil(t, node)
	})
}

func getCompositeNodeSelector(t *testing.T, region string, regions []config.RegionConfig, hardLimit float32) selector.NodeSelector {
	f1, err := selector.NewSimpleRegionAwareSelector(region, regions)
	require.NoError(t, err)

	f2 := &selector.SystemLoadSelector{
		SysloadLimit: loadLimit}
	if hardLimit != 0.0 {
		f2.HardSysloadLimit = hardLimit
	}
	return &selector.NodeSelectorBase{SortBy: sortBy, Selectors: []selector.NodeFilter{f1, f2}}
}
