package buffer

import "fmt"

const (
	InvalidLayerSpatial  = int32(-1)
	InvalidLayerTemporal = int32(-1)

	DefaultMaxLayerSpatial  = int32(2)
	DefaultMaxLayerTemporal = int32(3)
)

var (
	InvalidLayers = VideoLayer{
		Spatial:  InvalidLayerSpatial,
		Temporal: InvalidLayerTemporal,
	}
)

type VideoLayer struct {
	Spatial  int32
	Temporal int32
}

func (v VideoLayer) String() string {
	return fmt.Sprintf("VideoLayer{s: %d, t: %d}", v.Spatial, v.Temporal)
}

func (v VideoLayer) GreaterThan(v2 VideoLayer) bool {
	return v.Spatial > v2.Spatial || (v.Spatial == v2.Spatial && v.Temporal > v2.Temporal)
}

func (v VideoLayer) SpatialGreaterThanOrEqual(v2 VideoLayer) bool {
	return v.Spatial >= v2.Spatial
}

func (v VideoLayer) IsValid() bool {
	return v.Spatial != InvalidLayerSpatial && v.Temporal != InvalidLayerTemporal
}
