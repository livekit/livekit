// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package buffer

import "fmt"

const (
	InvalidLayerSpatial  = int32(-1)
	InvalidLayerTemporal = int32(-1)

	DefaultMaxLayerSpatial  = int32(2)
	DefaultMaxLayerTemporal = int32(3)
)

var (
	InvalidLayer = VideoLayer{
		Spatial:  InvalidLayerSpatial,
		Temporal: InvalidLayerTemporal,
	}

	DefaultMaxLayer = VideoLayer{
		Spatial:  DefaultMaxLayerSpatial,
		Temporal: DefaultMaxLayerTemporal,
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
