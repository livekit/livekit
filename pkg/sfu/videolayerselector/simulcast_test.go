// Copyright 2026 LiveKit, Inc.
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

package videolayerselector

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
)

func keyFrameOnLayer(spatial, temporal int32) *buffer.ExtPacket {
	return &buffer.ExtPacket{
		VideoLayer: buffer.VideoLayer{Spatial: spatial, Temporal: temporal},
		Packet:     &rtp.Packet{},
		IsKeyFrame: true,
	}
}

// On initial acquisition the selector must latch directly onto the target layer and ignore
// lower-layer key frames that arrive first, so a subscriber requesting the top layer does not
// briefly decode a lower layer (a visible quality ramp).
func TestSimulcastSelectAcquiresTargetLayerDirectly(t *testing.T) {
	s := NewSimulcast(logger.GetLogger())
	s.SetEnableStartAtDesiredQuality(true)
	s.SetMax(buffer.VideoLayer{Spatial: 2, Temporal: 2})
	s.SetMaxSeen(buffer.VideoLayer{Spatial: 2, Temporal: 2})
	s.SetTarget(buffer.VideoLayer{Spatial: 2, Temporal: 2})
	s.SetRequestSpatial(2)
	s.SetCurrent(buffer.InvalidLayer)

	// lower-layer key frames arriving first must NOT be latched
	require.False(t, s.Select(keyFrameOnLayer(0, 2), 0).IsSelected)
	require.False(t, s.GetCurrent().IsValid())
	require.False(t, s.Select(keyFrameOnLayer(1, 2), 1).IsSelected)
	require.False(t, s.GetCurrent().IsValid())

	// the target layer key frame latches directly
	require.True(t, s.Select(keyFrameOnLayer(2, 2), 2).IsSelected)
	require.Equal(t, int32(2), s.GetCurrent().Spatial)
}

// Acquisition follows whatever target the allocator set: when the target is lowered (e.g. the
// acquisition grace expired and the allocator fell back to the highest layer seen), the selector
// latches that lower layer directly. This is the fallback path that prevents a stall when the
// originally requested layer never shows up.
func TestSimulcastSelectAcquiresLoweredTarget(t *testing.T) {
	s := NewSimulcast(logger.GetLogger())
	s.SetEnableStartAtDesiredQuality(true)
	s.SetMax(buffer.VideoLayer{Spatial: 2, Temporal: 2})
	s.SetMaxSeen(buffer.VideoLayer{Spatial: 1, Temporal: 2})
	// allocator dropped the target to the highest layer actually seen
	s.SetTarget(buffer.VideoLayer{Spatial: 1, Temporal: 2})
	s.SetRequestSpatial(1)
	s.SetCurrent(buffer.InvalidLayer)

	require.False(t, s.Select(keyFrameOnLayer(0, 2), 0).IsSelected)
	require.False(t, s.GetCurrent().IsValid())

	require.True(t, s.Select(keyFrameOnLayer(1, 2), 1).IsSelected)
	require.Equal(t, int32(1), s.GetCurrent().Spatial)
}
