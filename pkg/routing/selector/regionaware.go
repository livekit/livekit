package selector

import (
	"math"

	livekit "github.com/livekit/protocol/proto"
	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/config"
)

// RegionAwareSelector prefers available nodes that are closest to the region of the current instance
type RegionAwareSelector struct {
	SystemLoadSelector
	CurrentRegion   string
	regionDistances map[string]float64
	regions         []config.RegionConfig
}

func NewRegionAwareSelector(currentRegion string, regions []config.RegionConfig) (*RegionAwareSelector, error) {
	if currentRegion == "" {
		return nil, ErrCurrentRegionNotSet
	}
	// build internal map of distances
	s := &RegionAwareSelector{
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

func (s *RegionAwareSelector) SelectNode(nodes []*livekit.Node) (*livekit.Node, error) {
	nodes, err := s.SystemLoadSelector.filterNodes(nodes)
	if err != nil {
		return nil, err
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

	idx := funk.RandomInt(0, len(nodes))
	return nodes[idx], nil
}

// haversin(θ) function
func hsin(theta float64) float64 {
	return math.Pow(math.Sin(theta/2), 2)
}

// Haversin Distance Formula
// http://en.wikipedia.org/wiki/Haversine_formula
// from https://gist.github.com/cdipaolo/d3f8db3848278b49db68
func distanceBetween(lat1, lon1, lat2, lon2 float64) float64 {
	// convert to radians
	// must cast radius as float to multiply later
	var la1, lo1, la2, lo2, r float64
	la1 = lat1 * math.Pi / 180
	lo1 = lon1 * math.Pi / 180
	la2 = lat2 * math.Pi / 180
	lo2 = lon2 * math.Pi / 180

	r = 6378100 // Earth radius in METERS

	// calculate
	h := hsin(la2-la1) + math.Cos(la1)*math.Cos(la2)*hsin(lo2-lo1)

	return 2 * r * math.Asin(math.Sqrt(h))
}
