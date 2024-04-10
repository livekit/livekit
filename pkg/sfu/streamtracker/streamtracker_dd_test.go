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

package streamtracker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

func createDescriptorDependencyForTargets(maxSpatial, maxTemporal int) *buffer.ExtDependencyDescriptor {
	var targets []buffer.DependencyDescriptorDecodeTarget
	var mask uint32
	for i := 0; i <= maxSpatial; i++ {
		for j := 0; j <= maxTemporal; j++ {
			targets = append(targets, buffer.DependencyDescriptorDecodeTarget{Target: len(targets), Layer: buffer.VideoLayer{Spatial: int32(i), Temporal: int32(j)}})
			mask |= 1 << uint32(len(targets)-1)
		}
	}

	dtis := make([]dd.DecodeTargetIndication, len(targets))
	for _, t := range targets {
		dtis[t.Target] = dd.DecodeTargetRequired
	}

	return &buffer.ExtDependencyDescriptor{
		Descriptor: &dd.DependencyDescriptor{
			ActiveDecodeTargetsBitmask: &mask,
			FrameDependencies: &dd.FrameDependencyTemplate{
				DecodeTargetIndications: dtis,
			},
		},
		DecodeTargets:              targets,
		ActiveDecodeTargetsUpdated: true,
	}
}

func checkStatues(t *testing.T, statuses []StreamStatus, expected StreamStatus, maxSpatial int) {
	for i := 0; i <= maxSpatial; i++ {
		require.Equal(t, expected, statuses[i])
	}

	for i := maxSpatial + 1; i < len(statuses); i++ {
		require.NotEqual(t, expected, statuses[i])
	}
}

func TestStreamTrackerDD(t *testing.T) {
	ddTracker := NewStreamTrackerDependencyDescriptor(StreamTrackerParams{
		BitrateReportInterval: 1 * time.Second,
		Logger:                logger.GetLogger(),
	})
	layeredTrackers := make([]StreamTrackerWorker, buffer.DefaultMaxLayerSpatial+1)
	statuses := make([]StreamStatus, buffer.DefaultMaxLayerSpatial+1)
	for i := 0; i <= int(buffer.DefaultMaxLayerSpatial); i++ {
		layeredTrack := ddTracker.LayeredTracker(int32(i))
		layer := i
		layeredTrack.OnStatusChanged(func(status StreamStatus) {
			statuses[layer] = status
		})
		layeredTrack.Start()
		layeredTrackers[i] = layeredTrack
	}
	defer ddTracker.Stop()

	// no active layers
	ddTracker.Observe(0, 1000, 1000, false, 0, nil)
	checkStatues(t, statuses, StreamStatusActive, int(buffer.InvalidLayerSpatial))

	// layer seen [0,1]
	ddTracker.Observe(0, 1000, 1000, false, 0, createDescriptorDependencyForTargets(1, 1))
	checkStatues(t, statuses, StreamStatusActive, 1)

	// layer seen [0,1,2]
	ddTracker.Observe(0, 1000, 1000, false, 0, createDescriptorDependencyForTargets(2, 1))
	checkStatues(t, statuses, StreamStatusActive, 2)

	// layer 2 gone, layer seen [0,1]
	ddTracker.Observe(0, 1000, 1000, false, 0, createDescriptorDependencyForTargets(1, 1))
	checkStatues(t, statuses, StreamStatusActive, 1)
}
