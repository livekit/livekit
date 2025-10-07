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

package sfu

import (
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
)

func disable(f *Forwarder) {
	f.vls.SetCurrent(buffer.InvalidLayer)
	f.vls.SetTarget(buffer.InvalidLayer)
}

func newForwarder(codec webrtc.RTPCodecCapability, kind webrtc.RTPCodecType) *Forwarder {
	f := NewForwarder(kind, logger.GetLogger(), true, nil)
	f.DetermineCodec(codec, nil, livekit.VideoLayer_MODE_UNUSED)
	return f
}

func TestForwarderMute(t *testing.T) {
	f := newForwarder(testutils.TestOpusCodec, webrtc.RTPCodecTypeAudio)
	require.False(t, f.IsMuted())
	muted := f.Mute(false, true)
	require.False(t, muted) // no change in mute state
	require.False(t, f.IsMuted())

	muted = f.Mute(true, false)
	require.False(t, muted)
	require.False(t, f.IsMuted())

	muted = f.Mute(true, true)
	require.True(t, muted)
	require.True(t, f.IsMuted())

	muted = f.Mute(false, true)
	require.True(t, muted)
	require.False(t, f.IsMuted())
}

func TestForwarderLayersAudio(t *testing.T) {
	f := newForwarder(testutils.TestOpusCodec, webrtc.RTPCodecTypeAudio)

	require.Equal(t, buffer.InvalidLayer, f.MaxLayer())

	require.Equal(t, buffer.InvalidLayer, f.CurrentLayer())
	require.Equal(t, buffer.InvalidLayer, f.TargetLayer())

	changed, maxLayer := f.SetMaxSpatialLayer(1)
	require.False(t, changed)
	require.Equal(t, buffer.InvalidLayer, maxLayer)

	changed, maxLayer = f.SetMaxTemporalLayer(1)
	require.False(t, changed)
	require.Equal(t, buffer.InvalidLayer, maxLayer)

	require.Equal(t, buffer.InvalidLayer, f.MaxLayer())
}

func TestForwarderLayersVideo(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	maxLayer := f.MaxLayer()
	expectedLayers := buffer.VideoLayer{Spatial: buffer.InvalidLayerSpatial, Temporal: buffer.DefaultMaxLayerTemporal}
	require.Equal(t, expectedLayers, maxLayer)

	require.Equal(t, buffer.InvalidLayer, f.CurrentLayer())
	require.Equal(t, buffer.InvalidLayer, f.TargetLayer())

	expectedLayers = buffer.VideoLayer{
		Spatial:  buffer.DefaultMaxLayerSpatial,
		Temporal: buffer.DefaultMaxLayerTemporal,
	}
	changed, maxLayer := f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	require.True(t, changed)
	require.Equal(t, expectedLayers, maxLayer)

	changed, maxLayer = f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial - 1)
	require.True(t, changed)
	expectedLayers = buffer.VideoLayer{
		Spatial:  buffer.DefaultMaxLayerSpatial - 1,
		Temporal: buffer.DefaultMaxLayerTemporal,
	}
	require.Equal(t, expectedLayers, maxLayer)
	require.Equal(t, expectedLayers, f.MaxLayer())

	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 1})
	changed, maxLayer = f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial - 1)
	require.False(t, changed)
	require.Equal(t, expectedLayers, maxLayer)
	require.Equal(t, expectedLayers, f.MaxLayer())

	changed, maxLayer = f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	require.False(t, changed)
	require.Equal(t, expectedLayers, maxLayer)

	changed, maxLayer = f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal - 1)
	require.True(t, changed)
	expectedLayers = buffer.VideoLayer{
		Spatial:  buffer.DefaultMaxLayerSpatial - 1,
		Temporal: buffer.DefaultMaxLayerTemporal - 1,
	}
	require.Equal(t, expectedLayers, maxLayer)
	require.Equal(t, expectedLayers, f.MaxLayer())
}

func TestForwarderAllocateOptimal(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	emptyBitrates := Bitrates{}
	bitrates := Bitrates{
		{2, 3, 0, 0},
		{4, 0, 0, 5},
		{0, 7, 0, 0},
	}

	// invalid max layers
	f.vls.SetMax(buffer.InvalidLayer)
	expectedResult := VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry,
		BandwidthRequested:  0,
		BandwidthDelta:      0,
		Bitrates:            bitrates,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.InvalidLayer,
		DistanceToDesired:   0,
	}
	result := f.AllocateOptimal(nil, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)

	// should still have target at buffer.InvalidLayer until max publisher layer is available
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry,
		BandwidthRequested:  0,
		BandwidthDelta:      0,
		Bitrates:            bitrates,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0,
	}
	result = f.AllocateOptimal(nil, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	f.SetMaxPublishedLayer(buffer.DefaultMaxLayerSpatial)

	// muted should not consume any bandwidth
	f.Mute(true, true)
	disable(f)
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonMuted,
		BandwidthRequested:  0,
		BandwidthDelta:      0,
		Bitrates:            bitrates,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0,
	}
	result = f.AllocateOptimal(nil, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	f.Mute(false, true)

	// pub muted should not consume any bandwidth
	f.PubMute(true)
	disable(f)
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonPubMuted,
		BandwidthRequested:  0,
		BandwidthDelta:      0,
		Bitrates:            bitrates,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0,
	}
	result = f.AllocateOptimal(nil, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	f.PubMute(false)

	// when max layers changes, target is opportunistic, but requested spatial layer should be at max
	f.SetMaxTemporalLayerSeen(buffer.DefaultMaxLayerTemporal)
	f.vls.SetMax(buffer.VideoLayer{Spatial: 1, Temporal: 3})
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[2][1],
		BandwidthDelta:      bitrates[2][1],
		BandwidthNeeded:     bitrates[1][3],
		Bitrates:            bitrates,
		TargetLayer:         buffer.DefaultMaxLayer,
		RequestLayerSpatial: f.vls.GetMax().Spatial,
		MaxLayer:            f.vls.GetMax(),
		DistanceToDesired:   -1,
	}
	result = f.AllocateOptimal(nil, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, buffer.DefaultMaxLayer, f.TargetLayer())

	// reset max layers for rest of the tests below
	f.vls.SetMax(buffer.DefaultMaxLayer)

	// when feed is dry and current is not valid, should set up for opportunistic forwarding
	// NOTE: feed is dry due to availableLayers = nil, some valid bitrates may be passed in here for testing purposes only
	disable(f)
	expectedTargetLayer := buffer.DefaultMaxLayer
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[2][1],
		BandwidthDelta:      0,
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   -0.5,
	}
	result = f.AllocateOptimal(nil, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})  // set to valid to trigger paths in tests below
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 3}) // set to valid to trigger paths in tests below

	// when feed is dry and current is valid, should stay at current
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  0,
		Temporal: 3,
	}
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry,
		BandwidthRequested:  0,
		BandwidthDelta:      0 - bitrates[2][1],
		Bitrates:            emptyBitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   -0.75,
	}
	result = f.AllocateOptimal(nil, emptyBitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	f.vls.SetCurrent(buffer.InvalidLayer)

	// opportunistic target if feed is not dry and current is not valid, i. e. not forwarding
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[2][1],
		BandwidthDelta:      bitrates[2][1],
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         buffer.DefaultMaxLayer,
		RequestLayerSpatial: 1,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   -0.5,
	}
	result = f.AllocateOptimal([]int32{0, 1}, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, buffer.DefaultMaxLayer, f.TargetLayer())

	// when holding in above scenario, should choose the lowest available layer
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  1,
		Temporal: 0,
	}
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[1][0],
		BandwidthDelta:      bitrates[1][0] - bitrates[2][1],
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: 1,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   1.25,
	}
	result = f.AllocateOptimal([]int32{1, 2}, bitrates, true, true)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	// opportunistic target if feed is dry and current is not valid, i. e. not forwarding
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[2][1],
		BandwidthDelta:      bitrates[2][1] - bitrates[1][0],
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         buffer.DefaultMaxLayer,
		RequestLayerSpatial: 2,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   -0.5,
	}
	result = f.AllocateOptimal(nil, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, buffer.DefaultMaxLayer, f.TargetLayer())

	// when holding in above scenario, should choose layer 0
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  0,
		Temporal: 0,
	}
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[0][0],
		BandwidthDelta:      bitrates[0][0] - bitrates[2][1],
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: 0,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   2.25,
	}
	result = f.AllocateOptimal(nil, bitrates, true, true)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	// if feed is not dry and current is not locked, should be opportunistic (with and without overshoot)
	f.vls.SetTarget(buffer.InvalidLayer)
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry,
		BandwidthRequested:  0,
		BandwidthDelta:      0 - bitrates[0][0],
		BandwidthNeeded:     0,
		Bitrates:            emptyBitrates,
		TargetLayer:         buffer.DefaultMaxLayer,
		RequestLayerSpatial: 1,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   -1.0,
	}
	result = f.AllocateOptimal([]int32{0, 1}, emptyBitrates, false, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, buffer.DefaultMaxLayer, f.TargetLayer())

	f.vls.SetTarget(buffer.InvalidLayer)
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  2,
		Temporal: buffer.DefaultMaxLayerTemporal,
	}
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[2][1],
		BandwidthDelta:      bitrates[2][1],
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: 1,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   -0.5,
	}
	result = f.AllocateOptimal([]int32{0, 1}, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	// switches request layer to highest available if feed is not dry and current is valid and current is not available
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 1})
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  1,
		Temporal: buffer.DefaultMaxLayerTemporal,
	}
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[1][3],
		BandwidthDelta:      bitrates[1][3] - bitrates[2][1],
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: 1,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0.5,
	}
	result = f.AllocateOptimal([]int32{1}, bitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	// when holding in above scenario, should switch to lowest available layer
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  0,
		Temporal: 0,
	}
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		BandwidthRequested:  bitrates[0][0],
		BandwidthDelta:      bitrates[0][0] - bitrates[1][3],
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: 0,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   2.25,
	}
	result = f.AllocateOptimal([]int32{0, 1}, bitrates, true, true)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	// stays the same if feed is not dry and current is valid, available and locked
	f.vls.SetMax(buffer.VideoLayer{Spatial: 0, Temporal: 1})
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 1})
	f.vls.SetRequestSpatial(0)
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  0,
		Temporal: 1,
	}
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry,
		BandwidthRequested:  0,
		BandwidthDelta:      0 - bitrates[0][0],
		Bitrates:            emptyBitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: 0,
		MaxLayer:            f.vls.GetMax(),
		DistanceToDesired:   0.0,
	}
	result = f.AllocateOptimal([]int32{0}, emptyBitrates, true, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
}

func TestForwarderProvisionalAllocate(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	f.SetMaxPublishedLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayerSeen(buffer.DefaultMaxLayerTemporal)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(nil, bitrates)

	isCandidate, usedBitrate := f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 0, Temporal: 0}, true, false)
	require.True(t, isCandidate)
	require.Equal(t, bitrates[0][0], usedBitrate)

	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 2, Temporal: 3}, true, false)
	require.True(t, isCandidate)
	require.Equal(t, bitrates[2][3]-bitrates[0][0], usedBitrate)

	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 0, Temporal: 3}, true, false)
	require.True(t, isCandidate)
	require.Equal(t, bitrates[0][3]-bitrates[2][3], usedBitrate)

	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 1, Temporal: 2}, true, false)
	require.True(t, isCandidate)
	require.Equal(t, bitrates[1][2]-bitrates[0][3], usedBitrate)

	// available not enough to reach (2, 2), allocating at (2, 2) should not succeed
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][2]-bitrates[1][2]-1, buffer.VideoLayer{Spatial: 2, Temporal: 2}, true, false)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	// committing should set target to (1, 2)
	expectedTargetLayer := buffer.VideoLayer{
		Spatial:  1,
		Temporal: 2,
	}
	expectedResult := VideoAllocation{
		IsDeficient:         true,
		BandwidthRequested:  bitrates[1][2],
		BandwidthDelta:      bitrates[1][2],
		BandwidthNeeded:     bitrates[2][3],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   1.25,
	}
	result := f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	// when nothing fits and pausing disallowed, should allocate (0, 0)
	f.vls.SetTarget(buffer.InvalidLayer)
	f.ProvisionalAllocatePrepare(nil, bitrates)
	isCandidate, usedBitrate = f.ProvisionalAllocate(0, buffer.VideoLayer{Spatial: 0, Temporal: 0}, false, false)
	require.True(t, isCandidate)
	require.Equal(t, int64(1), usedBitrate)

	// committing should set target to (0, 0)
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  0,
		Temporal: 0,
	}
	expectedResult = VideoAllocation{
		IsDeficient:         true,
		BandwidthRequested:  bitrates[0][0],
		BandwidthDelta:      bitrates[0][0] - bitrates[1][2],
		BandwidthNeeded:     bitrates[2][3],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   2.75,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	//
	// Test allowOvershoot.
	// Max spatial set to 0 and layer 0 bit rates are not available.
	//
	f.SetMaxSpatialLayer(0)
	bitrates = Bitrates{
		{0, 0, 0, 0},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(nil, bitrates)

	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 0, Temporal: 0}, false, true)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	// overshoot should succeed
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 2, Temporal: 3}, false, true)
	require.True(t, isCandidate)
	require.Equal(t, bitrates[2][3], usedBitrate)

	// overshoot should succeed - this should win as this is lesser overshoot
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 1, Temporal: 3}, false, true)
	require.True(t, isCandidate)
	require.Equal(t, bitrates[1][3]-bitrates[2][3], usedBitrate)

	// committing should set target to (1, 3)
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  1,
		Temporal: 3,
	}
	expectedMaxLayer := buffer.VideoLayer{
		Spatial:  0,
		Temporal: 3,
	}
	expectedResult = VideoAllocation{
		BandwidthRequested:  bitrates[1][3],
		BandwidthDelta:      bitrates[1][3] - 1, // 1 is the last allocation bandwidth requested
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            expectedMaxLayer,
		DistanceToDesired:   -1.75,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	//
	// Even if overshoot is allowed, but if higher layers do not have bit rates, should continue with current layer.
	//
	bitrates = Bitrates{
		{0, 0, 0, 0},
		{0, 0, 0, 0},
		{0, 0, 0, 0},
	}

	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 2})
	f.ProvisionalAllocatePrepare(nil, bitrates)

	// all the provisional allocations should not succeed because the feed is dry
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 0, Temporal: 0}, false, true)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	// overshoot should not succeed
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 2, Temporal: 3}, false, true)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	// overshoot should not succeed
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 1, Temporal: 3}, false, true)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	// committing should set target to (0, 2), i. e. leave it at current for opportunistic forwarding
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  0,
		Temporal: 2,
	}
	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry,
		BandwidthRequested:  bitrates[0][2],
		BandwidthDelta:      bitrates[0][2] - 8, // 8 is the last allocation bandwidth requested
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            expectedMaxLayer,
		DistanceToDesired:   1.0,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())

	//
	// Same case as above, but current is above max, so target should go to invalid
	//
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 1, Temporal: 2})
	f.ProvisionalAllocatePrepare(nil, bitrates)

	// all the provisional allocations below should not succeed because the feed is dry
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 0, Temporal: 0}, false, true)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	// overshoot should not succeed
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 2, Temporal: 3}, false, true)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	// overshoot should not succeed
	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 1, Temporal: 3}, false, true)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	expectedResult = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry,
		BandwidthRequested:  0,
		BandwidthDelta:      0,
		Bitrates:            bitrates,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            expectedMaxLayer,
		DistanceToDesired:   1.0,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, buffer.InvalidLayer, f.TargetLayer())
	require.Equal(t, buffer.InvalidLayer, f.CurrentLayer())
}

func TestForwarderProvisionalAllocateMute(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.Mute(true, true)
	f.ProvisionalAllocatePrepare(nil, bitrates)

	isCandidate, usedBitrate := f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 0, Temporal: 0}, true, false)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	isCandidate, usedBitrate = f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 1, Temporal: 2}, true, true)
	require.False(t, isCandidate)
	require.Equal(t, int64(0), usedBitrate)

	// committing should set target to buffer.InvalidLayer as track is muted
	expectedResult := VideoAllocation{
		PauseReason:         VideoPauseReasonMuted,
		BandwidthRequested:  0,
		BandwidthDelta:      0,
		Bitrates:            bitrates,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0,
	}
	result := f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, buffer.InvalidLayer, f.TargetLayer())
}

func TestForwarderProvisionalAllocateGetCooperativeTransition(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	f.SetMaxPublishedLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayerSeen(buffer.DefaultMaxLayerTemporal)

	availableLayers := []int32{0, 1, 2}
	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 0, 0},
	}

	f.ProvisionalAllocatePrepare(availableLayers, bitrates)

	// from scratch (buffer.InvalidLayer) should give back layer (0, 0)
	expectedTransition := VideoTransition{
		From:           buffer.InvalidLayer,
		To:             buffer.VideoLayer{Spatial: 0, Temporal: 0},
		BandwidthDelta: 1,
	}
	transition, al, brs := f.ProvisionalAllocateGetCooperativeTransition(false)
	require.Equal(t, expectedTransition, transition)
	require.Equal(t, availableLayers, al)
	require.Equal(t, bitrates, brs)

	// committing should set target to (0, 0)
	expectedLayers := buffer.VideoLayer{Spatial: 0, Temporal: 0}
	expectedResult := VideoAllocation{
		IsDeficient:         true,
		BandwidthRequested:  1,
		BandwidthDelta:      1,
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedLayers,
		RequestLayerSpatial: expectedLayers.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   2.25,
	}
	result := f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedLayers, f.TargetLayer())

	// a higher target that is already streaming, just maintain it
	targetLayer := buffer.VideoLayer{Spatial: 2, Temporal: 1}
	f.vls.SetTarget(targetLayer)
	f.lastAllocation.BandwidthRequested = 10
	expectedTransition = VideoTransition{
		From:           targetLayer,
		To:             targetLayer,
		BandwidthDelta: 0,
	}
	transition, al, brs = f.ProvisionalAllocateGetCooperativeTransition(false)
	require.Equal(t, expectedTransition, transition)
	require.Equal(t, availableLayers, al)
	require.Equal(t, bitrates, brs)

	// committing should set target to (2, 1)
	expectedLayers = buffer.VideoLayer{Spatial: 2, Temporal: 1}
	expectedResult = VideoAllocation{
		BandwidthRequested:  10,
		BandwidthDelta:      0,
		Bitrates:            bitrates,
		BandwidthNeeded:     bitrates[2][1],
		TargetLayer:         expectedLayers,
		RequestLayerSpatial: expectedLayers.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0.0,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedLayers, f.TargetLayer())

	// from a target that has become unavailable, should switch to lower available layer
	targetLayer = buffer.VideoLayer{Spatial: 2, Temporal: 2}
	f.vls.SetTarget(targetLayer)
	expectedTransition = VideoTransition{
		From:           targetLayer,
		To:             buffer.VideoLayer{Spatial: 2, Temporal: 1},
		BandwidthDelta: 0,
	}
	transition, al, brs = f.ProvisionalAllocateGetCooperativeTransition(false)
	require.Equal(t, expectedTransition, transition)
	require.Equal(t, availableLayers, al)
	require.Equal(t, bitrates, brs)

	f.ProvisionalAllocateCommit()

	// mute
	f.Mute(true, true)
	f.ProvisionalAllocatePrepare(availableLayers, bitrates)

	// mute should send target to buffer.InvalidLayer
	expectedTransition = VideoTransition{
		From:           buffer.VideoLayer{Spatial: 2, Temporal: 1},
		To:             buffer.InvalidLayer,
		BandwidthDelta: -10,
	}
	transition, al, brs = f.ProvisionalAllocateGetCooperativeTransition(false)
	require.Equal(t, expectedTransition, transition)
	require.Equal(t, availableLayers, al)
	require.Equal(t, bitrates, brs)

	f.ProvisionalAllocateCommit()

	//
	// Test allowOvershoot
	//
	f.Mute(false, true)
	f.SetMaxSpatialLayer(0)

	availableLayers = []int32{1, 2}
	bitrates = Bitrates{
		{0, 0, 0, 0},
		{5, 6, 7, 8},
		{9, 10, 0, 0},
	}

	f.vls.SetTarget(buffer.InvalidLayer)
	f.ProvisionalAllocatePrepare(availableLayers, bitrates)

	// from scratch (buffer.InvalidLayer) should go to a layer past maximum as overshoot is allowed
	expectedTransition = VideoTransition{
		From:           buffer.InvalidLayer,
		To:             buffer.VideoLayer{Spatial: 1, Temporal: 0},
		BandwidthDelta: 5,
	}
	transition, al, brs = f.ProvisionalAllocateGetCooperativeTransition(true)
	require.Equal(t, expectedTransition, transition)
	require.Equal(t, availableLayers, al)
	require.Equal(t, bitrates, brs)

	// committing should set target to (1, 0)
	expectedLayers = buffer.VideoLayer{Spatial: 1, Temporal: 0}
	expectedMaxLayer := buffer.VideoLayer{Spatial: 0, Temporal: buffer.DefaultMaxLayerTemporal}
	expectedResult = VideoAllocation{
		BandwidthRequested:  5,
		BandwidthDelta:      5,
		Bitrates:            bitrates,
		TargetLayer:         expectedLayers,
		RequestLayerSpatial: expectedLayers.Spatial,
		MaxLayer:            expectedMaxLayer,
		DistanceToDesired:   -1.0,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedLayers, f.TargetLayer())

	//
	// Test continuing at current layers when feed is dry
	//
	bitrates = Bitrates{
		{0, 0, 0, 0},
		{0, 0, 0, 0},
		{0, 0, 0, 0},
	}

	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 2})
	f.vls.SetTarget(buffer.InvalidLayer)
	f.ProvisionalAllocatePrepare(nil, bitrates)

	// from scratch (buffer.InvalidLayer) should go to current layer
	// NOTE: targetLayer is set to buffer.InvalidLayer for testing, but in practice current layers valid and target layers invalid should not happen
	expectedTransition = VideoTransition{
		From:           buffer.InvalidLayer,
		To:             buffer.VideoLayer{Spatial: 0, Temporal: 2},
		BandwidthDelta: -5, // 5 was the bandwidth needed for the last allocation
	}
	transition, al, brs = f.ProvisionalAllocateGetCooperativeTransition(true)
	require.Equal(t, expectedTransition, transition)
	require.Equal(t, []int32{}, al)
	require.Equal(t, bitrates, brs)

	// committing should set target to (0, 2)
	expectedLayers = buffer.VideoLayer{Spatial: 0, Temporal: 2}
	expectedResult = VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      -5,
		Bitrates:            bitrates,
		TargetLayer:         expectedLayers,
		RequestLayerSpatial: expectedLayers.Spatial,
		MaxLayer:            expectedMaxLayer,
		DistanceToDesired:   -0.5,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedLayers, f.TargetLayer())

	// committing should set target to current layers to enable opportunistic forwarding
	expectedResult = VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      0,
		Bitrates:            bitrates,
		TargetLayer:         expectedLayers,
		RequestLayerSpatial: expectedLayers.Spatial,
		MaxLayer:            expectedMaxLayer,
		DistanceToDesired:   -0.5,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedLayers, f.TargetLayer())
}

func TestForwarderProvisionalAllocateGetBestWeightedTransition(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)

	availableLayers := []int32{0, 1, 2}
	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(availableLayers, bitrates)

	f.vls.SetTarget(buffer.VideoLayer{Spatial: 2, Temporal: 2})
	f.lastAllocation.BandwidthRequested = bitrates[2][2]
	expectedTransition := VideoTransition{
		From:           f.TargetLayer(),
		To:             buffer.VideoLayer{Spatial: 2, Temporal: 0},
		BandwidthDelta: -2,
	}
	transition, al, brs := f.ProvisionalAllocateGetBestWeightedTransition()
	require.Equal(t, expectedTransition, transition)
	require.Equal(t, availableLayers, al)
	require.Equal(t, bitrates, brs)
}

func TestForwarderAllocateNextHigher(t *testing.T) {
	f := newForwarder(testutils.TestOpusCodec, webrtc.RTPCodecTypeAudio)
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	f.SetMaxPublishedLayer(buffer.DefaultMaxLayerSpatial)

	emptyBitrates := Bitrates{}
	bitrates := Bitrates{
		{2, 3, 0, 0},
		{4, 0, 0, 5},
		{0, 7, 0, 0},
	}

	result, boosted := f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, VideoAllocationDefault, result) // no layer for audio
	require.False(t, boosted)

	f = newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	f.SetMaxPublishedLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayerSeen(buffer.DefaultMaxLayerTemporal)

	// when not in deficient state, does not boost
	result, boosted = f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, VideoAllocationDefault, result)
	require.False(t, boosted)

	// if layers have not caught up, should not allocate next layer even if deficient
	f.vls.SetTarget(buffer.VideoLayer{
		Spatial:  0,
		Temporal: 0,
	})
	result, boosted = f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, VideoAllocationDefault, result)
	require.False(t, boosted)

	f.lastAllocation.IsDeficient = true
	f.vls.SetCurrent(buffer.VideoLayer{
		Spatial:  0,
		Temporal: 0,
	})

	// move from (0, 0) -> (0, 1), i.e. a higher temporal layer is available in the same spatial layer
	expectedTargetLayer := buffer.VideoLayer{
		Spatial:  0,
		Temporal: 1,
	}
	expectedResult := VideoAllocation{
		IsDeficient:         true,
		BandwidthRequested:  3,
		BandwidthDelta:      1,
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   2.0,
	}
	result, boosted = f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
	require.True(t, boosted)

	// empty bitrates cannot increase layer, i. e. last allocation is left unchanged
	result, boosted = f.AllocateNextHigher(100_000_000, nil, emptyBitrates, false)
	require.Equal(t, expectedResult, result)
	require.False(t, boosted)

	// move from (0, 1) -> (1, 0), i.e. a higher spatial layer is available
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: f.vls.GetCurrent().Spatial, Temporal: 1})
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  1,
		Temporal: 0,
	}
	expectedResult = VideoAllocation{
		IsDeficient:         true,
		BandwidthRequested:  4,
		BandwidthDelta:      1,
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   1.25,
	}
	result, boosted = f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
	require.True(t, boosted)

	// next higher, move from (1, 0) -> (1, 3), still deficient though
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 1, Temporal: 0})
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  1,
		Temporal: 3,
	}
	expectedResult = VideoAllocation{
		IsDeficient:         true,
		BandwidthRequested:  5,
		BandwidthDelta:      1,
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0.5,
	}
	result, boosted = f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
	require.True(t, boosted)

	// next higher, move from (1, 3) -> (2, 1), optimal allocation
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: f.vls.GetCurrent().Spatial, Temporal: 3})
	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  2,
		Temporal: 1,
	}
	expectedResult = VideoAllocation{
		BandwidthRequested:  7,
		BandwidthDelta:      2,
		Bitrates:            bitrates,
		BandwidthNeeded:     bitrates[2][1],
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0.0,
	}
	result, boosted = f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
	require.True(t, boosted)

	// ask again, should return not boosted as there is no room to go higher
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 2, Temporal: 1})
	result, boosted = f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
	require.False(t, boosted)

	// turn off everything, allocating next layer should result in streaming lowest layers
	disable(f)
	f.lastAllocation.IsDeficient = true
	f.lastAllocation.BandwidthRequested = 0

	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  0,
		Temporal: 0,
	}
	expectedResult = VideoAllocation{
		IsDeficient:         true,
		BandwidthRequested:  2,
		BandwidthDelta:      2,
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   2.25,
	}
	result, boosted = f.AllocateNextHigher(100_000_000, nil, bitrates, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
	require.True(t, boosted)

	// no new available capacity cannot bump up layer
	expectedResult = VideoAllocation{
		IsDeficient:         true,
		BandwidthRequested:  2,
		BandwidthDelta:      2,
		BandwidthNeeded:     bitrates[2][1],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   2.25,
	}
	result, boosted = f.AllocateNextHigher(0, nil, bitrates, false)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
	require.False(t, boosted)

	// test allowOvershoot
	f.SetMaxSpatialLayer(0)

	bitrates = Bitrates{
		{0, 0, 0, 0},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.vls.SetCurrent(f.vls.GetTarget())

	expectedTargetLayer = buffer.VideoLayer{
		Spatial:  1,
		Temporal: 0,
	}
	expectedMaxLayer := buffer.VideoLayer{
		Spatial:  0,
		Temporal: buffer.DefaultMaxLayerTemporal,
	}
	expectedResult = VideoAllocation{
		BandwidthRequested:  bitrates[1][0],
		BandwidthDelta:      bitrates[1][0],
		Bitrates:            bitrates,
		TargetLayer:         expectedTargetLayer,
		RequestLayerSpatial: expectedTargetLayer.Spatial,
		MaxLayer:            expectedMaxLayer,
		DistanceToDesired:   -1.0,
	}
	// overshoot should return (1, 0) even if there is not enough capacity
	result, boosted = f.AllocateNextHigher(bitrates[1][0]-1, nil, bitrates, true)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayer, f.TargetLayer())
	require.True(t, boosted)
}

func TestForwarderPause(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	f.SetMaxPublishedLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayerSeen(buffer.DefaultMaxLayerTemporal)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(nil, bitrates)
	f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 0, Temporal: 0}, true, false)
	// should have set target at (0, 0)
	f.ProvisionalAllocateCommit()

	expectedResult := VideoAllocation{
		PauseReason:         VideoPauseReasonBandwidth,
		IsDeficient:         true,
		BandwidthRequested:  0,
		BandwidthDelta:      0 - bitrates[0][0],
		BandwidthNeeded:     bitrates[2][3],
		Bitrates:            bitrates,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   3.75,
	}
	result := f.Pause(nil, bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, buffer.InvalidLayer, f.TargetLayer())
}

func TestForwarderPauseMute(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	f.SetMaxPublishedLayer(buffer.DefaultMaxLayerSpatial)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(nil, bitrates)
	f.ProvisionalAllocate(bitrates[2][3], buffer.VideoLayer{Spatial: 0, Temporal: 0}, true, true)
	// should have set target at (0, 0)
	f.ProvisionalAllocateCommit()

	f.Mute(true, true)
	expectedResult := VideoAllocation{
		PauseReason:         VideoPauseReasonMuted,
		BandwidthRequested:  0,
		BandwidthDelta:      0 - bitrates[0][0],
		Bitrates:            bitrates,
		TargetLayer:         buffer.InvalidLayer,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayer:            buffer.DefaultMaxLayer,
		DistanceToDesired:   0,
	}
	result := f.Pause(nil, bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, buffer.InvalidLayer, f.TargetLayer())
}

func TestForwarderGetTranslationParamsMuted(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.Mute(true, true)

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, err := testutils.GetTestExtPacket(params)
	require.NoError(t, err)
	require.NotNil(t, extPkt)

	expectedTP := TranslationParams{
		shouldDrop: true,
	}
	actualTP, err := f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)
}

func TestForwarderGetTranslationParamsAudio(t *testing.T) {
	f := newForwarder(testutils.TestOpusCodec, webrtc.RTPCodecTypeAudio)

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
		IsOutOfOrder:   true,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)

	// should not start on an out-of-order packet
	expectedTP := TranslationParams{
		shouldDrop: true,
	}
	actualTP, err := f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)
	require.False(t, f.started)
	require.Zero(t, f.lastSSRC)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	// should lock onto the first in-order packet
	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23333,
			extTimestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)
	require.True(t, f.started)
	require.Equal(t, f.lastSSRC, params.SSRC)

	// send a duplicate, should be dropped
	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// add a missing sequence number to the cache
	err = f.rtpMunger.snRangeMap.ExcludeRange(23334, 23335)
	require.NoError(t, err)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23336,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	_, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)

	// out-of-order packet should get offset from cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23335,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingOutOfOrder,
			extSequenceNumber: 23334,
			extTimestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// padding only packet in order should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23337,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// in order packet should be forwarded
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23338,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23336,
			extTimestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// padding only packet after a gap should not be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23340,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingGap,
			extSequenceNumber: 23338,
			extTimestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// out-of-order should be forwarded using cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23336,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingOutOfOrder,
			extSequenceNumber: 23335,
			extTimestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// switching source should lock onto the new source, but sequence number should be contiguous
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 123,
		Timestamp:      0xfedcba,
		SSRC:           0x87654321,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23339,
			extTimestamp:      0xabcdf0,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)
	require.Equal(t, f.lastSSRC, params.SSRC)
}

func TestForwarderGetTranslationParamsVideo(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
		Marker:         true,
		IsOutOfOrder:   true,
	}
	vp8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: false,
	}
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)

	// should not start on an out-of-order packet
	expectedTP := TranslationParams{
		shouldDrop: true,
	}
	actualTP, err := f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)
	require.False(t, f.started)
	require.Zero(t, f.lastSSRC)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
		Marker:         true,
	}
	vp8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: false,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)

	// no target layers, should drop
	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// although target layer matches, not a key frame, so should drop
	f.vls.SetTarget(buffer.VideoLayer{
		Spatial:  0,
		Temporal: 1,
	})
	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// should lock onto packet (key frame)
	vp8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedVP8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	marshalledVP8, err := expectedVP8.Marshal()
	expectedTP = TranslationParams{
		isSwitching: true,
		isResuming:  true,
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23333,
			extTimestamp:      0xabcdef,
		},
		incomingHeaderSize: 6,
		codecBytes:         marshalledVP8,
		marker:             true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)
	require.True(t, f.started)
	require.Equal(t, f.lastSSRC, params.SSRC)

	// send a duplicate, should be dropped
	expectedTP = TranslationParams{
		shouldDrop: true,
		marker:     true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// out-of-order packet not in cache should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// padding only packet in order should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23334,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// in order packet should be forwarded
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23335,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedVP8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	marshalledVP8, err = expectedVP8.Marshal()
	require.NoError(t, err)
	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23334,
			extTimestamp:      0xabcdef,
		},
		incomingHeaderSize: 6,
		codecBytes:         marshalledVP8,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// temporal layer matching target, should be forwarded
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23336,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 = &buffer.VP8{
		FirstByte:  25,
		S:          true,
		I:          true,
		M:          true,
		PictureID:  13468,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        1,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedVP8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13468,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        1,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	marshalledVP8, err = expectedVP8.Marshal()
	require.NoError(t, err)
	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23335,
			extTimestamp:      0xabcdef,
		},
		incomingHeaderSize: 6,
		codecBytes:         marshalledVP8,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// temporal layer higher than target, should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23337,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13468,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        2,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedTP = TranslationParams{
		shouldDrop: true,
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23336,
			extTimestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// RTP sequence number and VP8 picture id should be contiguous after dropping higher temporal layer picture
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23338,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13469,
		L:          true,
		TL0PICIDX:  234,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: false,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedVP8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13469,
		L:          true,
		TL0PICIDX:  234,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: false,
	}
	marshalledVP8, err = expectedVP8.Marshal()
	require.NoError(t, err)
	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23336,
			extTimestamp:      0xabcdef,
		},
		incomingHeaderSize: 6,
		codecBytes:         marshalledVP8,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// padding only packet after a gap should be forwarded
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23340,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingGap,
			extSequenceNumber: 23338,
			extTimestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// out-of-order should be forwarded using cache, even if it is padding only
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23339,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingOutOfOrder,
			extSequenceNumber: 23337,
			extTimestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)

	// switching SSRC (happens for new layer or new track source)
	// should lock onto the new source, but sequence number should be contiguous
	f.vls.SetTarget(buffer.VideoLayer{
		Spatial:  1,
		Temporal: 1,
	})

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 123,
		Timestamp:      0xfedcba,
		SSRC:           0x87654321,
		PayloadSize:    20,
	}
	vp8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          false,
		PictureID:  45,
		L:          true,
		TL0PICIDX:  12,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     30,
		HeaderSize: 5,
		IsKeyFrame: true,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)

	expectedVP8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13470,
		L:          true,
		TL0PICIDX:  235,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     24,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	marshalledVP8, err = expectedVP8.Marshal()
	require.NoError(t, err)
	expectedTP = TranslationParams{
		isSwitching: true,
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23339,
			extTimestamp:      0xabcdf0,
		},
		incomingHeaderSize: 5,
		codecBytes:         marshalledVP8,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 1)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)
	require.Equal(t, f.lastSSRC, params.SSRC)
}

func TestForwarderGetSnTsForPadding(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)

	f.vls.SetTarget(buffer.VideoLayer{
		Spatial:  0,
		Temporal: 1,
	})
	f.vls.SetCurrent(buffer.InvalidLayer)

	// send it through so that forwarder locks onto stream
	_, _ = f.GetTranslationParams(extPkt, 0)

	// pause stream and get padding, it should still work
	disable(f)

	// should get back frame end needed as the last packet did not have RTP marker set
	snts, err := f.GetSnTsForPadding(5, 0, false)
	require.NoError(t, err)

	numPadding := 5
	clockRate := uint32(0)
	frameRate := uint32(5)
	var sntsExpected = make([]SnTs, numPadding)
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			extSequenceNumber: 23333 + uint64(i) + 1,
			extTimestamp:      0xabcdef + (uint64(i)*uint64(clockRate))/uint64(frameRate),
		}
	}
	require.Equal(t, sntsExpected, snts)

	// now that there is a marker, timestamp should jump on first padding when asked again
	snts, err = f.GetSnTsForPadding(numPadding, 0, false)
	require.NoError(t, err)

	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			extSequenceNumber: 23338 + uint64(i) + 1,
			extTimestamp:      0xabcdef + (uint64(i+1)*uint64(clockRate))/uint64(frameRate),
		}
	}
	require.Equal(t, sntsExpected, snts)
}

func TestForwarderGetSnTsForBlankFrames(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)

	f.vls.SetTarget(buffer.VideoLayer{
		Spatial:  0,
		Temporal: 1,
	})
	f.vls.SetCurrent(buffer.InvalidLayer)

	// send it through so that forwarder locks onto stream
	_, _ = f.GetTranslationParams(extPkt, 0)

	// should get back frame end needed as the last packet did not have RTP marker set
	numBlankFrames := 6
	snts, frameEndNeeded, err := f.GetSnTsForBlankFrames(30, numBlankFrames)
	require.NoError(t, err)
	require.True(t, frameEndNeeded)

	// there should be one more than RTPBlankFramesMax as one would have been allocated to end previous frame
	numPadding := numBlankFrames + 1
	clockRate := testutils.TestVP8Codec.ClockRate
	frameRate := uint32(30)
	var sntsExpected = make([]SnTs, numPadding)
	for i := 0; i < numPadding; i++ {
		// first blank frame should have same timestamp as last frame as end frame is synthesized
		ts := params.Timestamp
		if i != 0 {
			// +1 here due to expected time stamp bumpint by at least one so that time stamp is always moving ahead
			ts = params.Timestamp + 1 + ((uint32(i)*clockRate)+frameRate-1)/frameRate
		}
		sntsExpected[i] = SnTs{
			extSequenceNumber: uint64(params.SequenceNumber) + uint64(i) + 1,
			extTimestamp:      uint64(ts),
		}
	}
	require.Equal(t, sntsExpected, snts)

	// now that there is a marker, timestamp should jump on first padding when asked again
	// also number of padding should be RTPBlankFramesMax
	numPadding = numBlankFrames
	sntsExpected = sntsExpected[:numPadding]
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			extSequenceNumber: uint64(params.SequenceNumber) + uint64(len(snts)) + uint64(i) + 1,
			// +1 here due to expected time stamp bumpint by at least one so that time stamp is always moving ahead
			extTimestamp: snts[len(snts)-1].extTimestamp + 1 + ((uint64(i+1)*uint64(clockRate))+uint64(frameRate)-1)/uint64(frameRate),
		}
	}
	snts, frameEndNeeded, err = f.GetSnTsForBlankFrames(30, numBlankFrames)
	require.NoError(t, err)
	require.False(t, frameEndNeeded)
	require.Equal(t, sntsExpected, snts)
}

func TestForwarderGetPaddingVP8(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        13,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)

	f.vls.SetTarget(buffer.VideoLayer{
		Spatial:  0,
		Temporal: 1,
	})
	f.vls.SetCurrent(buffer.InvalidLayer)

	// send it through so that forwarder locks onto stream
	_, _ = f.GetTranslationParams(extPkt, 0)

	// getting padding with frame end needed, should repeat the last picture id
	expectedVP8 := buffer.VP8{
		FirstByte:  16,
		I:          true,
		M:          true,
		PictureID:  13467,
		L:          true,
		TL0PICIDX:  233,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     23,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	buf, err := f.GetPadding(true)
	require.NoError(t, err)
	marshalledVP8, err := expectedVP8.Marshal()
	require.NoError(t, err)
	require.Equal(t, marshalledVP8, buf)

	// getting padding with no frame end needed, should get next picture id
	expectedVP8 = buffer.VP8{
		FirstByte:  16,
		I:          true,
		M:          true,
		PictureID:  13468,
		L:          true,
		TL0PICIDX:  234,
		T:          true,
		TID:        0,
		Y:          true,
		K:          true,
		KEYIDX:     24,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	buf, err = f.GetPadding(false)
	require.NoError(t, err)
	marshalledVP8, err = expectedVP8.Marshal()
	require.NoError(t, err)
	require.Equal(t, marshalledVP8, buf)
}
