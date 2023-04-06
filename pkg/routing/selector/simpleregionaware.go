package selector

import (
	"github.com/livekit/protocol/livekit"
	"math"

	"github.com/livekit/livekit-server/pkg/config"
)

// SimpleRegionAwareSelector prefers available nodes that are closest to the region of the current instance.
// It can be chained with other Node selectors
type SimpleRegionAwareSelector struct {
	CurrentRegion   string
	regionDistances map[string]float64
	regions         []config.RegionConfig
}

func NewSimpleRegionAwareSelector(currentRegion string, regions []config.RegionConfig) (*SimpleRegionAwareSelector, error) {
	if currentRegion == "" {
		return nil, ErrCurrentRegionNotSet
	}
	// build internal map of distances
	s := &SimpleRegionAwareSelector{
		CurrentRegion:   currentRegion,
		regionDistances: make(map[string]float64),
		regions:         regions,
	}

	var currentRC *config.RegionConfig

	for _, region := range regions {
		if region.Name == currentRegion {
			currentRC = &region
			break
		}
	}

	if currentRC == nil && len(regions) > 0 {
		return nil, ErrCurrentRegionUnknownLatLon
	}

	if currentRC != nil {
		for _, region := range regions {
			s.regionDistances[region.Name] = distanceBetween(currentRC.Lat, currentRC.Lon, region.Lat, region.Lon)
		}
	}

	return s, nil
}

func (s *SimpleRegionAwareSelector) FilterNodes(nodes []*livekit.Node, selectionType NodeSelection) ([]*livekit.Node, error) {
	if len(nodes) == 0 {
		return nil, ErrNoAvailableNodes
	}
	// find nodes nearest to current region
	var nearestNodes []*livekit.Node
	nearestRegion := ""
	minDist := math.MaxFloat64
	for _, node := range nodes {
		if node.Region == nearestRegion {
			nearestNodes = append(nearestNodes, node)
			continue
		}
		if dist, ok := s.regionDistances[node.Region]; ok {
			if dist < minDist {
				minDist = dist
				nearestRegion = node.Region
				nearestNodes = nearestNodes[:0]
				nearestNodes = append(nearestNodes, node)
			}
		}
	}

	if len(nearestNodes) > 0 {
		nodes = nearestNodes
	}
	return nodes, nil
}
