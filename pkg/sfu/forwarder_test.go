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
	"encoding/hex"
	"math/rand"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/mediatransportutil/pkg/codec"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/codecmunger"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector"
)

func disable(f *Forwarder) {
	f.vls.SetCurrent(buffer.InvalidLayer)
	f.vls.SetTarget(buffer.InvalidLayer)
}

func newForwarder(codec webrtc.RTPCodecCapability, kind webrtc.RTPCodecType) *Forwarder {
	f := NewForwarder(
		kind,
		logger.GetLogger(),
		true, // skipReferenceTS
		true, // disableOpportunisticAllocation
		true, // enableStartAtDesiredQuality
		nil,
	)
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

	// Reset to invalid layers for testing allocation from scratch
	disable(f)

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

	// Reset to invalid layers for testing muted state
	disable(f)

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

	// Reset to invalid layers for testing cooperative transition from scratch
	disable(f)

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
		isStarting: true,
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
	vp8 := &codec.VP8{
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
	vp8 = &codec.VP8{
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
	vp8 = &codec.VP8{
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
	expectedVP8 := &codec.VP8{
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
	marshalledVP8, _ := expectedVP8.Marshal()
	expectedTP = TranslationParams{
		isStarting:  true,
		isSwitching: true,
		isResuming:  true,
		rtp: TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingContiguous,
			extSequenceNumber: 23333,
			extTimestamp:      0xabcdef,
		},
		incomingHeaderSize: 6,
		codecBytes:         codecHeaderOf(marshalledVP8),
		codecBytesLen:      len(marshalledVP8),
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
	expectedVP8 = &codec.VP8{
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
		codecBytes:         codecHeaderOf(marshalledVP8),
		codecBytesLen:      len(marshalledVP8),
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
	vp8 = &codec.VP8{
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
	expectedVP8 = &codec.VP8{
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
		codecBytes:         codecHeaderOf(marshalledVP8),
		codecBytesLen:      len(marshalledVP8),
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
	vp8 = &codec.VP8{
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
	vp8 = &codec.VP8{
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
	expectedVP8 = &codec.VP8{
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
		codecBytes:         codecHeaderOf(marshalledVP8),
		codecBytesLen:      len(marshalledVP8),
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
	vp8 = &codec.VP8{
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

	expectedVP8 = &codec.VP8{
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
		codecBytes:         codecHeaderOf(marshalledVP8),
		codecBytesLen:      len(marshalledVP8),
	}
	actualTP, err = f.GetTranslationParams(extPkt, 1)
	require.NoError(t, err)
	require.Equal(t, expectedTP, actualTP)
	require.Equal(t, f.lastSSRC, params.SSRC)
}

func TestForwarderVP8TemporalUpSwitchAtLayerSync(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 2})

	// a frame without Y may reference a dropped frame of its layer
	steps := []struct {
		name            string
		tid             uint8
		y               bool
		expectedThis    int32
		expectedCurrent int32
	}{
		{name: "TL2 without Y", tid: 2, expectedThis: 0, expectedCurrent: 0},
		{name: "TL1 without Y", tid: 1, expectedThis: 0, expectedCurrent: 0},
		{name: "TL0", tid: 0, expectedThis: 0, expectedCurrent: 0},
		{name: "TL2 with Y before TL1 sync", tid: 2, y: true, expectedThis: 0, expectedCurrent: 0},
		{name: "TL1 with Y", tid: 1, y: true, expectedThis: 1, expectedCurrent: 1},
		{name: "TL2 without Y after TL1 switch", tid: 2, expectedThis: 1, expectedCurrent: 1},
		{name: "TL2 with Y", tid: 2, y: true, expectedThis: 2, expectedCurrent: 2},
	}
	for i, step := range steps {
		extPkt, err := testutils.GetTestExtPacketVP8(
			&testutils.TestExtPacketParams{SequenceNumber: uint16(i), PayloadSize: 20},
			&codec.VP8{S: true, I: true, PictureID: uint16(i), T: true, TID: step.tid, Y: step.y},
		)
		require.NoError(t, err)
		require.Equal(t, step.expectedThis, f.vls.SelectTemporal(extPkt), step.name)
		require.Equal(t, step.expectedCurrent, f.vls.GetCurrent().Temporal, step.name)
	}

	// key frame refreshes all buffers, up-switch at it even without Y
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	extPkt, err := testutils.GetTestExtPacketVP8(
		&testutils.TestExtPacketParams{SequenceNumber: 10, PayloadSize: 20},
		&codec.VP8{S: true, I: true, PictureID: 10, T: true, TID: 0, IsKeyFrame: true},
	)
	require.NoError(t, err)
	require.Equal(t, int32(2), f.vls.SelectTemporal(extPkt))
	require.Equal(t, int32(2), f.vls.GetCurrent().Temporal)
}

func TestForwarderVP8TemporalUpSwitchWithDependencyDescriptor(t *testing.T) {
	f := NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
	f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	require.IsType(t, &codecmunger.DDTemporal{}, f.codecMunger)
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 2})

	// libwebrtc strips the VP8 payload descriptor when it sends the dependency
	// descriptor: no TID and no layer sync bit
	packet := func(frameNum uint64, tid int, dtis string, frameDiffs []int, isKeyFrame bool) *buffer.ExtPacket {
		extPkt, err := testutils.GetTestExtPacketVP8DD(
			&testutils.TestExtPacketParams{SequenceNumber: uint16(frameNum), PayloadSize: 20, Marker: true, IsKeyFrame: isKeyFrame},
			&testutils.TestDDParams{
				FirstPacketInFrame: true,
				ExtFrameNum:        frameNum,
				TemporalID:         tid,
				DTIs:               dtis,
				FrameDiffs:         frameDiffs,
				DecodeTargets:      testutils.TestVP8DecodeTargets,
			},
		)
		require.NoError(t, err)
		return extPkt
	}

	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	require.Equal(t, int32(0), f.vls.SelectTemporal(packet(0, 0, "SSS", nil, true)))
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 2})
	require.Equal(t, int32(0), f.vls.SelectTemporal(packet(1, 2, "--D", []int{1}, false)))
	require.Equal(t, int32(0), f.vls.GetCurrent().Temporal)
	require.Equal(t, int32(2), f.vls.SelectTemporal(packet(2, 1, "-SS", []int{2}, false)))
	require.Equal(t, int32(2), f.vls.GetCurrent().Temporal)

	// padding never carries the descriptor and leaves it on
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	padding, err := testutils.GetTestExtPacket(&testutils.TestExtPacketParams{SequenceNumber: 3, PaddingSize: 20})
	require.NoError(t, err)
	_, _ = f.getTranslationParamsVideo(padding, 0)
	require.True(t, f.isDDAvailable)

	// a packet with a payload and without the descriptor means the buffer stopped
	// parsing it, the Y bit then raises the layer
	extPkt, err := testutils.GetTestExtPacketVP8(
		&testutils.TestExtPacketParams{SequenceNumber: 4, PayloadSize: 20},
		&codec.VP8{S: true, I: true, PictureID: 4, T: true, TID: 0},
	)
	require.NoError(t, err)
	_, _ = f.getTranslationParamsVideo(extPkt, 0)
	require.False(t, f.isDDAvailable)
	require.IsType(t, &codecmunger.VP8{}, f.codecMunger)

	extPkt, err = testutils.GetTestExtPacketVP8(
		&testutils.TestExtPacketParams{SequenceNumber: 5, PayloadSize: 20},
		&codec.VP8{S: true, I: true, PictureID: 5, T: true, TID: 1, Y: true},
	)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.vls.SelectTemporal(extPkt))
	require.Equal(t, int32(1), f.vls.GetCurrent().Temporal)
}

func TestForwarderVP8DDTemporalFilterAfterGap(t *testing.T) {
	f := NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
	f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})

	// one packet per frame, the minimized VP8 payload descriptor has no picture ID
	packet := func(sn uint16, frameNum uint64, tid int, isKeyFrame bool) *buffer.ExtPacket {
		extPkt, err := testutils.GetTestExtPacketVP8DD(
			&testutils.TestExtPacketParams{
				SequenceNumber: sn,
				Timestamp:      uint32(frameNum) * 3000,
				SSRC:           0x12345678,
				PayloadSize:    20,
				Marker:         true,
				IsKeyFrame:     isKeyFrame,
			},
			&testutils.TestDDParams{
				FirstPacketInFrame: true,
				ExtFrameNum:        frameNum,
				TemporalID:         tid,
				DTIs:               "SSS",
				DecodeTargets:      testutils.TestVP8DecodeTargets,
			},
		)
		require.NoError(t, err)
		return extPkt
	}

	steps := []struct {
		name       string
		sn         uint16
		frameNum   uint64
		tid        int
		isKeyFrame bool
		shouldDrop bool
	}{
		{name: "key frame", sn: 100, frameNum: 0, tid: 0, isKeyFrame: true},
		{name: "TL2 after a gap", sn: 102, frameNum: 2, tid: 2, shouldDrop: true},
		{name: "TL2", sn: 103, frameNum: 3, tid: 2, shouldDrop: true},
		{name: "TL0", sn: 104, frameNum: 4, tid: 0},
		{name: "TL2 after TL0", sn: 105, frameNum: 5, tid: 2, shouldDrop: true},
	}
	for _, step := range steps {
		tp, err := f.GetTranslationParams(packet(step.sn, step.frameNum, step.tid, step.isKeyFrame), 0)
		require.NoError(t, err, step.name)
		require.Equal(t, step.shouldDrop, tp.shouldDrop, step.name)
	}
}

func TestForwarderVP8DDWithPictureID(t *testing.T) {
	f := NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
	f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 2})
	require.IsType(t, &codecmunger.DDTemporal{}, f.codecMunger)

	// the one byte descriptor keeps the dependency descriptor munger
	extPkt, err := testutils.GetTestExtPacketVP8DD(
		&testutils.TestExtPacketParams{SequenceNumber: 1, Timestamp: 3000, SSRC: 0x12345678, PayloadSize: 20, Marker: true, IsKeyFrame: true},
		&testutils.TestDDParams{FirstPacketInFrame: true, ExtFrameNum: 1, DTIs: "SSS", DecodeTargets: testutils.TestVP8DecodeTargets},
	)
	require.NoError(t, err)
	_, _ = f.GetTranslationParams(extPkt, 0)
	require.IsType(t, &codecmunger.DDTemporal{}, f.codecMunger)
	require.True(t, f.started)

	// a picture ID next to the dependency descriptor needs the VP8 munger
	extPkt, err = testutils.GetTestExtPacketVP8DD(
		&testutils.TestExtPacketParams{SequenceNumber: 2, Timestamp: 6000, SSRC: 0x12345678, PayloadSize: 20, Marker: true},
		&testutils.TestDDParams{FirstPacketInFrame: true, ExtFrameNum: 2, TemporalID: 1, DTIs: "-SS", FrameDiffs: []int{1}, DecodeTargets: testutils.TestVP8DecodeTargets},
	)
	require.NoError(t, err)
	extPkt.VP8 = &codec.VP8{S: true, I: true, PictureID: 2}
	_, _ = f.GetTranslationParams(extPkt, 0)
	require.IsType(t, &codecmunger.VP8{}, f.codecMunger)
	require.True(t, f.codecMunger.GetState().(*livekit.VP8MungerState).PictureIdUsed, "seeded from the packet")
	require.True(t, f.isDDAvailable, "temporal layers still switch with the dependency descriptor")
}

// testKeyFrameDescriptor is an L3T3 key frame descriptor with its dependency structure, the
// capture the dependency descriptor package tests with
func testKeyFrameDescriptor(t *testing.T) dd.DependencyDescriptor {
	raw, err := hex.DecodeString("c1017280081485214eafffaaaa863cf0430c10c302afc0aaa0063c00430010c002a000a80006000040001d954926e082b04a0941b820ac1282503157f974000ca864330e222222eca8655304224230eca877530077004200ef008601df010d")
	require.NoError(t, err)
	keyFrameDescriptor := dd.DependencyDescriptor{}
	_, err = (&dd.DependencyDescriptorExtension{Descriptor: &keyFrameDescriptor}).Unmarshal(raw)
	require.NoError(t, err)
	require.NotNil(t, keyFrameDescriptor.AttachedStructure)
	return keyFrameDescriptor
}

// testDDPacket is a packet of frame frameNum on temporalID, with keyFrameDescriptor as its
// dependency descriptor, its structure attached only on a key frame
func testDDPacket(t *testing.T, keyFrameDescriptor dd.DependencyDescriptor, sn uint16, frameNum uint64, temporalID int, isKeyFrame bool) *buffer.ExtPacket {
	extPkt, err := testutils.GetTestExtPacketVP8DD(
		&testutils.TestExtPacketParams{SequenceNumber: sn, Timestamp: 0xabcdef + uint32(frameNum)*3000, SSRC: 0x12345678, PayloadSize: 20, Marker: true, IsKeyFrame: isKeyFrame},
		&testutils.TestDDParams{FirstPacketInFrame: true, ExtFrameNum: frameNum, TemporalID: temporalID, DTIs: "SSS", DecodeTargets: testutils.TestVP8DecodeTargets},
	)
	require.NoError(t, err)
	descriptor := keyFrameDescriptor
	descriptor.FrameNumber = uint16(frameNum)
	if !isKeyFrame {
		descriptor.AttachedStructure = nil
		descriptor.ActiveDecodeTargetsBitmask = nil
	}
	extPkt.DependencyDescriptor.Descriptor = &descriptor
	return extPkt
}

func TestForwarderVP8ForwardsDependencyDescriptor(t *testing.T) {
	keyFrameDescriptor := testKeyFrameDescriptor(t)
	packet := func(sn uint16, frameNum uint64, isKeyFrame bool) *buffer.ExtPacket {
		return testDDPacket(t, keyFrameDescriptor, sn, frameNum, 0, isKeyFrame)
	}

	f := NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
	f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})

	// the key frame carries the structure, too big to hold inline
	tp, err := f.GetTranslationParams(packet(10, 100, true), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.NotEmpty(t, tp.ddBytesSpill)

	tp, err = f.GetTranslationParams(packet(11, 101, false), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.NotEmpty(t, tp.ddInline())

	// a restarted upstream receiver sets the codec again, the frame numbers continue on
	// the same downtrack
	f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	tp, err = f.GetTranslationParams(packet(12, 50, true), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	out := dd.DependencyDescriptor{}
	_, err = (&dd.DependencyDescriptorExtension{Descriptor: &out, Structure: keyFrameDescriptor.AttachedStructure}).Unmarshal(tp.ddBytesSpill)
	require.NoError(t, err)
	require.Equal(t, uint16(102), out.FrameNumber)

	// the last packet of a TL2 frame that starts a down switch to TL0 is forwarded at TL2,
	// its descriptor names the TL2 decode target
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 2})
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	tl2 := packet(13, 51, false)
	tl2.Temporal = 2
	tp, err = f.GetTranslationParams(tl2, 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.Equal(t, int32(0), f.vls.GetCurrent().Temporal, "the switch takes effect after this packet")
	out = dd.DependencyDescriptor{}
	_, err = (&dd.DependencyDescriptorExtension{Descriptor: &out, Structure: keyFrameDescriptor.AttachedStructure}).Unmarshal(tp.ddInline())
	require.NoError(t, err)
	require.Equal(t, uint32(1<<0|1<<1|1<<2), *out.ActiveDecodeTargetsBitmask)

	// a migrated subscriber: the state is seeded before the codec is known, and the new
	// node's first key frame continues after the last number sent
	state := f.GetState()
	require.Equal(t, uint64(103), state.DependencyDescriptorForwarder.ExtLastFrameNumber)
	f = NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
	f.SeedState(state)

	// it migrates on before its codec is known, the next node still gets the number
	state = f.GetState()
	require.Equal(t, uint64(103), state.DependencyDescriptorForwarder.ExtLastFrameNumber)
	f = NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
	f.SeedState(state)
	f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	tp, err = f.GetTranslationParams(packet(20, 60, true), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	out = dd.DependencyDescriptor{}
	_, err = (&dd.DependencyDescriptorExtension{Descriptor: &out, Structure: keyFrameDescriptor.AttachedStructure}).Unmarshal(tp.ddBytesSpill)
	require.NoError(t, err)
	require.Equal(t, uint16(104), out.FrameNumber)

	// without the extension the forwarder writes no descriptor
	f = NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
	f.DetermineCodec(testutils.TestVP8Codec, nil, livekit.VideoLayer_MODE_UNUSED)
	require.Nil(t, f.ddForwarder)
}

func TestForwarderPadDroppedSlot(t *testing.T) {
	f := NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
	f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
	f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})

	packet := func(sn uint16, frameNum uint64, tid int, isOutOfOrder bool) *buffer.ExtPacket {
		extPkt, err := testutils.GetTestExtPacketVP8DD(
			&testutils.TestExtPacketParams{SequenceNumber: sn, Timestamp: 0xabcdef, SSRC: 0x12345678, PayloadSize: 20, Marker: true, IsOutOfOrder: isOutOfOrder},
			&testutils.TestDDParams{
				FirstPacketInFrame: true,
				ExtFrameNum:        frameNum,
				TemporalID:         tid,
				DTIs:               "SSS",
				DecodeTargets:      testutils.TestVP8DecodeTargets,
			},
		)
		require.NoError(t, err)
		return extPkt
	}

	tp, err := f.GetTranslationParams(packet(10, 0, 0, false), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)

	// a gap, the munger hands out a slot for sequence number 11
	tp, err = f.GetTranslationParams(packet(12, 2, 0, false), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.Equal(t, uint64(12), tp.rtp.extSequenceNumber)

	// the late packet is on a filtered layer, its slot gets padding
	tp, err = f.GetTranslationParams(packet(11, 1, 2, true), 0)
	require.NoError(t, err)
	require.True(t, tp.shouldDrop)
	require.True(t, tp.padDroppedSlot)
	require.Equal(t, uint64(11), tp.rtp.extSequenceNumber)
	require.Equal(t, uint64(0xabcdef), tp.rtp.extTimestamp)

	// an in-order drop rolls the slot back instead
	tp, err = f.GetTranslationParams(packet(13, 3, 2, false), 0)
	require.NoError(t, err)
	require.True(t, tp.shouldDrop)
	require.False(t, tp.padDroppedSlot)
	tp, err = f.GetTranslationParams(packet(14, 4, 0, false), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.Equal(t, uint64(13), tp.rtp.extSequenceNumber)
}

// dropSelector drops one packet as relevant, the result a decode target selector gives for a
// frame outside the target
type dropSelector struct {
	videolayerselector.VideoLayerSelector
	dropSN uint64
}

func (d *dropSelector) Select(extPkt *buffer.ExtPacket, layer int32) videolayerselector.VideoLayerSelectorResult {
	if extPkt.ExtSequenceNumber == d.dropSN {
		return videolayerselector.VideoLayerSelectorResult{IsRelevant: true}
	}
	return d.VideoLayerSelector.Select(extPkt, layer)
}

func TestForwarderPadFilteredSlotAfterGap(t *testing.T) {
	for _, tc := range []struct {
		name           string
		dropBySelector bool
	}{
		{"codec munger", false},
		{"layer selector", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
			f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
			f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
			f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})
			if tc.dropBySelector {
				f.vls = &dropSelector{VideoLayerSelector: f.vls, dropSN: 12}
			}

			packet := func(sn uint16, frameNum uint64, tid int, isOutOfOrder bool) *buffer.ExtPacket {
				extPkt, err := testutils.GetTestExtPacketVP8DD(
					&testutils.TestExtPacketParams{SequenceNumber: sn, Timestamp: 0xabcdef + uint32(frameNum)*3000, SSRC: 0x12345678, PayloadSize: 20, Marker: true, IsOutOfOrder: isOutOfOrder},
					&testutils.TestDDParams{
						FirstPacketInFrame: true,
						ExtFrameNum:        frameNum,
						TemporalID:         tid,
						DTIs:               "SSS",
						DecodeTargets:      testutils.TestVP8DecodeTargets,
					},
				)
				require.NoError(t, err)
				return extPkt
			}

			tp, err := f.GetTranslationParams(packet(10, 0, 0, false), 0)
			require.NoError(t, err)
			require.False(t, tp.shouldDrop)

			// 11 is lost, 12 is on a filtered layer: its slot gets padding and is not rolled back
			tp, err = f.GetTranslationParams(packet(12, 2, 2, false), 0)
			require.NoError(t, err)
			require.True(t, tp.shouldDrop)
			require.True(t, tp.padDroppedSlot)
			require.Equal(t, uint64(12), tp.rtp.extSequenceNumber)

			// the retransmission of 11 still has its slot
			tp, err = f.GetTranslationParams(packet(11, 1, 0, true), 0)
			require.NoError(t, err)
			require.False(t, tp.shouldDrop)
			require.Equal(t, uint64(11), tp.rtp.extSequenceNumber)

			// the next packet follows the padded slot
			tp, err = f.GetTranslationParams(packet(13, 3, 0, false), 0)
			require.NoError(t, err)
			require.False(t, tp.shouldDrop)
			require.Equal(t, uint64(13), tp.rtp.extSequenceNumber)
		})
	}
}

func TestForwarderGetSnTsForPadding(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 := &codec.VP8{
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
	for i := range numPadding {
		sntsExpected[i] = SnTs{
			extSequenceNumber: 23333 + uint64(i) + 1,
			extTimestamp:      0xabcdef + (uint64(i)*uint64(clockRate))/uint64(frameRate),
		}
	}
	require.Equal(t, sntsExpected, snts)

	// now that there is a marker, timestamp should jump on first padding when asked again
	snts, err = f.GetSnTsForPadding(numPadding, 0, false)
	require.NoError(t, err)

	for i := range numPadding {
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
	vp8 := &codec.VP8{
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
	vp8 := &codec.VP8{
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
	expectedVP8 := codec.VP8{
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
	expectedVP8 = codec.VP8{
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

func TestForwarderInitialAcquisitionGrace(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	// subscriber requested the top spatial layer
	f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	f.SetMaxTemporalLayerSeen(buffer.DefaultMaxLayerTemporal)

	bitrates := Bitrates{
		{2, 3, 0, 0},
		{4, 0, 0, 5},
		{0, 7, 0, 0},
	}

	// subscriber-first: only layer 1 has been seen so far and nothing is being forwarded yet
	// (current invalid). this arms the initial-acquisition grace.
	require.True(t, f.SetMaxPublishedLayer(1))
	f.lock.RLock()
	require.True(t, f.withinAcquireGraceLocked())
	f.lock.RUnlock()

	// during the grace, the target aims straight at the requested layer (2) and requests a key
	// frame for it, even though only layer 1 has been seen - so acquisition does not ramp up
	// gradually as higher layers are detected
	alloc := f.AllocateOptimal([]int32{0, 1}, bitrates, false, false)
	require.Equal(t, int32(2), alloc.TargetLayer.Spatial)
	require.Equal(t, int32(2), alloc.RequestLayerSpatial)

	// force the grace to expire while still not streaming: must signal that a re-allocation is
	// needed so the target can fall back
	f.acquireDeadline = 1 // a deadline far in the past
	require.True(t, f.MaybeExpireAcquireGrace())
	require.False(t, f.MaybeExpireAcquireGrace()) // only fires once

	// after the grace, the target falls back to the highest layer actually seen (1) instead of
	// stalling while waiting for a requested layer that never showed up
	alloc = f.AllocateOptimal([]int32{0, 1}, bitrates, false, false)
	require.Equal(t, int32(1), alloc.TargetLayer.Spatial)
	require.Equal(t, int32(1), alloc.RequestLayerSpatial)
}

func TestGetRefLayerRTPTimestampBounds(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	layerCount := int32(len(f.refInfos))

	_, err := f.getRefLayerRTPTimestamp(1000, layerCount, 0)
	require.Error(t, err)

	_, err = f.getRefLayerRTPTimestamp(1000, 0, layerCount)
	require.Error(t, err)

	_, err = f.getRefLayerRTPTimestamp(1000, -1, 0)
	require.Error(t, err)

	_, err = f.getRefLayerRTPTimestamp(1000, 0, -1)
	require.Error(t, err)

	// same-layer translation does not need sender reports
	ts, err := f.getRefLayerRTPTimestamp(1000, 0, 0)
	require.NoError(t, err)
	require.Equal(t, uint32(1000), ts)

	// last valid index must be accepted by the bounds check (may still error for missing SR)
	_, err = f.getRefLayerRTPTimestamp(1000, layerCount-1, 0)
	require.Error(t, err) // unavailable sender report, not invalid layer
	require.Contains(t, err.Error(), "unavailable")
}

// TestForwarderIsEndOfLayerFrame checks the end-of-layer-frame detection used to
// locate packet trailers, which VP9 SVC can carry at the end of any spatial layer
// frame and not just at the end of a picture.
func TestForwarderIsEndOfLayerFrame(t *testing.T) {
	vp9Codec := webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP9.String(), ClockRate: 90000}

	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	require.False(t, f.isEndOfLayerFrame(&buffer.ExtPacket{VP9: &codecs.VP9Packet{E: true}}))

	f = newForwarder(vp9Codec, webrtc.RTPCodecTypeVideo)
	require.True(t, f.isEndOfLayerFrame(&buffer.ExtPacket{VP9: &codecs.VP9Packet{E: true}}))
	require.False(t, f.isEndOfLayerFrame(&buffer.ExtPacket{VP9: &codecs.VP9Packet{E: false}}))
	require.False(t, f.isEndOfLayerFrame(&buffer.ExtPacket{}))

	require.True(t, f.isEndOfLayerFrame(&buffer.ExtPacket{
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{
			Descriptor: &dd.DependencyDescriptor{LastPacketInFrame: true},
		},
		VP9: &codecs.VP9Packet{E: false},
	}))
	require.False(t, f.isEndOfLayerFrame(&buffer.ExtPacket{
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{
			Descriptor: &dd.DependencyDescriptor{LastPacketInFrame: false},
		},
		VP9: &codecs.VP9Packet{E: true},
	}))
	require.False(t, f.isEndOfLayerFrame(&buffer.ExtPacket{
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{},
	}))
}

func codecHeaderOf(b []byte) [codecmunger.MaxHeaderSize]byte {
	var hdr [codecmunger.MaxHeaderSize]byte
	copy(hdr[:], b)
	return hdr
}

// WriteRTP holds the dependency descriptor in pacer scratch sized to pion's one
// byte extension profile payload cap, which has to be the inline size
// TranslationParams carries, or descriptors would take the heap copy fallback
func TestPacerScratchFitsInlineDependencyDescriptor(t *testing.T) {
	p := &pacer.Packet{}

	require.Len(t, p.HoldExtension(make([]byte, dd.MaxInlineExtensionSize)), dd.MaxInlineExtensionSize)
	require.Nil(t, p.HoldExtension(make([]byte, dd.MaxInlineExtensionSize+1)))
}

func TestForwarderNeedsPeriodicSyncCheck(t *testing.T) {
	f := newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	require.False(t, f.NeedsPeriodicSyncCheck())

	// VP8 with the dependency descriptor uses it only for temporal layers
	f.DetermineCodec(testutils.TestVP8Codec, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	require.False(t, f.NeedsPeriodicSyncCheck())

	// a new max published layer before any layer is forwarded arms the acquisition grace
	require.True(t, f.SetMaxPublishedLayer(1))
	require.True(t, f.NeedsPeriodicSyncCheck())

	// spatial layers selected with the dependency descriptor
	f = newForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	av1 := webrtc.RTPCodecCapability{MimeType: mime.MimeTypeAV1.String(), ClockRate: 90000}
	f.DetermineCodec(av1, []webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}}, livekit.VideoLayer_MODE_UNUSED)
	require.True(t, f.NeedsPeriodicSyncCheck())
}

// TestForwarderSimulcastTemporalLayerAfterSwitch checks that, after a spatial layer switch on a
// key frame (which is always on temporal layer 0), the forwarder reaches its target layer so that
// GetNextHigherTransition (and hence stream allocator probing) is not blocked forever.
//
// VP9, AV1 and H.264 simulcast without the dependency descriptor forward every temporal layer of
// the selected spatial layer, so the current temporal layer has to follow the target. VP8 steps
// up through its temporal layer selector.
func TestForwarderSimulcastTemporalLayerAfterSwitch(t *testing.T) {
	bitrates := Bitrates{
		{100, 150, 200, 0},
		{300, 450, 600, 0},
		{900, 1350, 1800, 0},
	}

	testCases := []struct {
		name            string
		previousCodec   *webrtc.RTPCodecCapability
		codec           webrtc.RTPCodecCapability
		target          buffer.VideoLayer
		expectedCurrent buffer.VideoLayer
	}{
		{
			name:            "VP9 simulcast, three temporal layers",
			codec:           webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP9.String(), ClockRate: 90000},
			target:          buffer.VideoLayer{Spatial: 1, Temporal: 2},
			expectedCurrent: buffer.VideoLayer{Spatial: 1, Temporal: 2},
		},
		{
			name:            "AV1 simulcast, three temporal layers",
			codec:           webrtc.RTPCodecCapability{MimeType: mime.MimeTypeAV1.String(), ClockRate: 90000},
			target:          buffer.VideoLayer{Spatial: 1, Temporal: 2},
			expectedCurrent: buffer.VideoLayer{Spatial: 1, Temporal: 2},
		},
		{
			name:            "VP9 simulcast, one temporal layer",
			codec:           webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP9.String(), ClockRate: 90000},
			target:          buffer.VideoLayer{Spatial: 1, Temporal: 0},
			expectedCurrent: buffer.VideoLayer{Spatial: 1, Temporal: 0},
		},
		{
			name:            "H.264 simulcast",
			codec:           webrtc.RTPCodecCapability{MimeType: mime.MimeTypeH264.String(), ClockRate: 90000},
			target:          buffer.VideoLayer{Spatial: 1, Temporal: 0},
			expectedCurrent: buffer.VideoLayer{Spatial: 1, Temporal: 0},
		},
		{
			// the VP8 temporal layer selector moves to the target on a key frame
			name:            "VP8 simulcast, three temporal layers",
			codec:           testutils.TestVP8Codec,
			target:          buffer.VideoLayer{Spatial: 1, Temporal: 2},
			expectedCurrent: buffer.VideoLayer{Spatial: 1, Temporal: 2},
		},
		{
			// the VP8 temporal layer selector must not be carried over on codec change
			name:            "VP8 to VP9 simulcast, three temporal layers",
			previousCodec:   &testutils.TestVP8Codec,
			codec:           webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP9.String(), ClockRate: 90000},
			target:          buffer.VideoLayer{Spatial: 1, Temporal: 2},
			expectedCurrent: buffer.VideoLayer{Spatial: 1, Temporal: 2},
		},
		{
			name:            "VP8 to AV1 simulcast, three temporal layers",
			previousCodec:   &testutils.TestVP8Codec,
			codec:           webrtc.RTPCodecCapability{MimeType: mime.MimeTypeAV1.String(), ClockRate: 90000},
			target:          buffer.VideoLayer{Spatial: 1, Temporal: 2},
			expectedCurrent: buffer.VideoLayer{Spatial: 1, Temporal: 2},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewForwarder(
				webrtc.RTPCodecTypeVideo,
				logger.GetLogger(),
				true,  // skipReferenceTS
				true,  // disableOpportunisticAllocation
				false, // enableStartAtDesiredQuality
				nil,
			)
			if tc.previousCodec != nil {
				f.DetermineCodec(*tc.previousCodec, nil, livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM)
			}
			f.DetermineCodec(tc.codec, nil, livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM)
			f.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
			f.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
			f.SetMaxPublishedLayer(buffer.DefaultMaxLayerSpatial)
			f.SetMaxTemporalLayerSeen(buffer.DefaultMaxLayerTemporal)

			// down-switched for bandwidth: was on the top layer, target is now lower
			f.lastAllocation.IsDeficient = true
			f.vls.SetCurrent(buffer.VideoLayer{Spatial: 2, Temporal: tc.target.Temporal})
			f.vls.SetTarget(tc.target)

			// target pending (spatial switch not done yet): no higher transition
			_, available := f.GetNextHigherTransition(bitrates, false)
			require.False(t, available)

			// key frame of the target spatial layer, temporal layer 0
			params := &testutils.TestExtPacketParams{
				SequenceNumber: 23333,
				Timestamp:      0xabcdef,
				SSRC:           0x12345678,
				PayloadSize:    20,
				IsKeyFrame:     true,
				VideoLayer:     buffer.VideoLayer{Spatial: tc.target.Spatial, Temporal: 0},
			}
			var extPkt *buffer.ExtPacket
			var err error
			if tc.codec.MimeType == testutils.TestVP8Codec.MimeType {
				extPkt, err = testutils.GetTestExtPacketVP8(params, &codec.VP8{
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
				})
			} else {
				extPkt, err = testutils.GetTestExtPacket(params)
			}
			require.NoError(t, err)

			tp, err := f.GetTranslationParams(extPkt, tc.target.Spatial)
			require.NoError(t, err)
			require.False(t, tp.shouldDrop)
			require.True(t, tp.isSwitching)

			require.Equal(t, tc.expectedCurrent, f.vls.GetCurrent())
			// target reached: the next higher transition (and hence probing) is available
			_, available = f.GetNextHigherTransition(bitrates, false)
			require.True(t, available)
		})
	}
}

// VP9 and AV1 simulcast with the dependency descriptor filter temporal layers by frame and
// forward the descriptor, as VP8 does. A publisher that stops sending the descriptor gets
// every temporal layer forwarded.
func TestForwarderSimulcastDependencyDescriptor(t *testing.T) {
	for _, mimeType := range []mime.MimeType{mime.MimeTypeVP9, mime.MimeTypeAV1} {
		t.Run(mimeType.String(), func(t *testing.T) {
			keyFrameDescriptor := testKeyFrameDescriptor(t)
			f := NewForwarder(webrtc.RTPCodecTypeVideo, logger.GetLogger(), true, true, true, nil)
			f.DetermineCodec(
				webrtc.RTPCodecCapability{MimeType: mimeType.String(), ClockRate: 90000},
				[]webrtc.RTPHeaderExtensionParameter{{URI: dd.ExtensionURI}},
				livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM,
			)
			require.IsType(t, &codecmunger.DDTemporal{}, f.codecMunger)
			require.NotNil(t, f.ddForwarder)
			f.vls.SetCurrent(buffer.VideoLayer{Spatial: 0, Temporal: 0})
			f.vls.SetTarget(buffer.VideoLayer{Spatial: 0, Temporal: 0})

			tp, err := f.GetTranslationParams(testDDPacket(t, keyFrameDescriptor, 10, 100, 0, true), 0)
			require.NoError(t, err)
			require.False(t, tp.shouldDrop)
			require.NotEmpty(t, tp.ddBytesSpill)

			// TL2 is above the target and is filtered, its slot is rolled back
			tp, err = f.GetTranslationParams(testDDPacket(t, keyFrameDescriptor, 11, 101, 2, false), 0)
			require.NoError(t, err)
			require.True(t, tp.shouldDrop)

			tp, err = f.GetTranslationParams(testDDPacket(t, keyFrameDescriptor, 12, 102, 0, false), 0)
			require.NoError(t, err)
			require.False(t, tp.shouldDrop)
			require.Equal(t, uint64(11), tp.rtp.extSequenceNumber)
			require.NotEmpty(t, tp.ddInline())

			// the publisher stops sending the descriptor
			noDD, err := testutils.GetTestExtPacket(&testutils.TestExtPacketParams{SequenceNumber: 13, Timestamp: 0xabcdef + 103*3000, SSRC: 0x12345678, PayloadSize: 20, Marker: true})
			require.NoError(t, err)
			tp, err = f.GetTranslationParams(noDD, 0)
			require.NoError(t, err)
			require.False(t, tp.shouldDrop)
			require.Nil(t, f.ddForwarder)
			require.IsType(t, &codecmunger.Null{}, f.codecMunger)
			require.Empty(t, tp.ddInline())
		})
	}
}

// ---------------------------------------------------------------------------
// Switching back to a layer after the reference layer's RTP timestamp wrapped,
// on a downtrack that started with dummy forwarding (dummyStartTSOffset != 0).

const (
	wrapTestSSRCLow  = uint32(0x0a0a0a0a)
	wrapTestSSRCHigh = uint32(0x0c0c0c0c)
	wrapTestFrame    = uint32(3000) // 30 fps at 90 kHz
	wrapTestHighTS   = uint32(0x10000000)
)

func newWrapTestForwarder(t *testing.T, skipReferenceTS bool) *Forwarder {
	t.Helper()
	f := NewForwarder(
		webrtc.RTPCodecTypeVideo,
		logger.GetLogger(),
		skipReferenceTS,
		false, // disableOpportunisticAllocation
		false, // enableStartAtDesiredQuality
		rtpstats.NewRTPStatsSender(rtpstats.RTPStatsParams{}, 1024),
	)
	f.DetermineCodec(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, nil, livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM)
	f.SetMaxSpatialLayer(2)
	f.SetMaxTemporalLayer(0)
	f.SetMaxPublishedLayer(2)
	return f
}

func wrapTestBitrates(layer int32) Bitrates {
	var brs Bitrates
	brs[layer][0] = 1_000_000
	return brs
}

func wrapTestKeyFrame(t *testing.T, ssrc uint32, sn uint16, ts uint32, tsCycles int, layer int32) *buffer.ExtPacket {
	t.Helper()
	extPkt, err := testutils.GetTestExtPacket(&testutils.TestExtPacketParams{
		SequenceNumber: sn,
		Timestamp:      ts,
		TSCycles:       tsCycles,
		SSRC:           ssrc,
		PayloadSize:    20,
		Marker:         true,
		IsKeyFrame:     true,
		VideoLayer:     buffer.VideoLayer{Spatial: layer, Temporal: 0},
	})
	require.NoError(t, err)
	return extPkt
}

func wrapTestSenderReport(at time.Time, rtpTS uint32) *livekit.RTCPSenderReportState {
	return &livekit.RTCPSenderReportState{
		RtpTimestamp:    rtpTS,
		RtpTimestampExt: uint64(rtpTS),
		NtpTimestamp:    uint64(mediatransportutil.ToNtpTime(at)),
		At:              at.UnixNano(),
		AtAdjusted:      at.UnixNano(),
	}
}

// runToHighLayer starts the forwarder with dummy forwarding, locks it onto layer 0 (whose first
// timestamp is lowStart), switches it to layer 2 when layer 0 stops, forwards layer 2 for
// `elapsed`, and then has the allocator target layer 0 again. It returns layer 0's timestamp and
// timestamp cycles at that point, for the keyframe that switches back.
func runToHighLayer(t *testing.T, f *Forwarder, lowStart uint32, elapsed time.Duration) (uint32, int) {
	t.Helper()

	// dummy start, then lock onto layer 0: layer 0 becomes the reference layer
	f.maybeStart()
	f.AllocateOptimal([]int32{0}, wrapTestBitrates(0), true, false)
	tp, err := f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCLow, 1000, lowStart, 0, 0), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.Equal(t, int32(0), f.referenceLayerSpatial)
	require.Equal(t, int32(0), f.CurrentLayer().Spatial)

	now := time.Now()
	f.SetRefSenderReport(0, wrapTestSenderReport(now, lowStart+wrapTestFrame))
	f.SetRefSenderReport(2, wrapTestSenderReport(now, wrapTestHighTS+wrapTestFrame))

	// layer 0 stops, layer 2 is available: switch to layer 2
	alloc := f.AllocateOptimal([]int32{2}, wrapTestBitrates(2), true, false)
	require.Equal(t, int32(2), alloc.TargetLayer.Spatial)
	tp, err = f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCHigh, 5000, wrapTestHighTS+2*wrapTestFrame, 0, 2), 2)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.Equal(t, int32(2), f.CurrentLayer().Spatial)

	// forward layer 2 for `elapsed`
	ticks := uint64(elapsed.Seconds() * 90000)
	highTS := uint64(wrapTestHighTS) + ticks
	tp, err = f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCHigh, 5001, uint32(highTS), int(highTS>>32), 2), 2)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)

	// layer 2 stops, layer 0 is back: the allocator targets layer 0
	alloc = f.AllocateOptimal([]int32{0}, wrapTestBitrates(0), true, false)
	require.Equal(t, int32(0), alloc.TargetLayer.Spatial)

	lowTS := uint64(lowStart) + ticks + uint64(wrapTestFrame)
	return uint32(lowTS), int(lowTS >> 32)
}

func TestForwarderSwitchBackAfterReferenceTimestampWrap(t *testing.T) {
	// maybeStart draws the dummy first timestamp in [2^31, 2^31 + 2^30), so a reference layer
	// starting above 2^31 + 2^30 always yields a negative dummyStartTSOffset
	const lowStart = uint32(0xf0000000)

	t.Run("after the wrap", func(t *testing.T) {
		f := newWrapTestForwarder(t, false)
		lowTS, cycles := runToHighLayer(t, f, lowStart, time.Hour)

		// preconditions: negative offset, layer 0 wrapped and its 32-bit timestamp is below |offset|
		offset := int64(f.dummyStartTSOffset)
		require.Less(t, offset, int64(0))
		require.Equal(t, 1, cycles)
		require.Less(t, int64(lowTS), -offset)

		tp, err := f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCLow, 1001, lowTS, cycles, 0), 0)
		require.NoError(t, err)
		require.False(t, tp.shouldDrop, "switch point rejected")
		require.Equal(t, int32(0), f.CurrentLayer().Spatial)
	})

	t.Run("before the wrap", func(t *testing.T) {
		f := newWrapTestForwarder(t, false)
		lowTS, cycles := runToHighLayer(t, f, lowStart, 30*time.Minute)
		require.Equal(t, 0, cycles)

		tp, err := f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCLow, 1001, lowTS, cycles, 0), 0)
		require.NoError(t, err)
		require.False(t, tp.shouldDrop, "switch point rejected")
		require.Equal(t, int32(0), f.CurrentLayer().Spatial)
	})

	t.Run("random start and duration", func(t *testing.T) {
		rng := rand.New(rand.NewSource(1))
		for i := 0; i < 400; i++ {
			f := newWrapTestForwarder(t, false)
			start := rng.Uint32()
			elapsed := time.Duration(60+rng.Int63n(26*3600)) * time.Second
			lowTS, cycles := runToHighLayer(t, f, start, elapsed)

			tp, err := f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCLow, 1001, lowTS, cycles, 0), 0)
			require.NoError(t, err)
			require.False(t, tp.shouldDrop, "switch point rejected, start %d, elapsed %s", start, elapsed)
		}
	})
}

// Receiver restart on a downtrack that started with dummy forwarding: the new reference layer's
// timestamps must keep mapping onto the downtrack's existing timeline on every switch after
// the restart, not only on the first one.
func TestForwarderRestartKeepsTimestampContinuity(t *testing.T) {
	f := newWrapTestForwarder(t, false)
	f.rtpStats.SetClockRate(90000)

	// dummy start, lock onto layer 0 with a reference timestamp that yields a negative offset
	f.maybeStart()
	f.AllocateOptimal([]int32{0}, wrapTestBitrates(0), true, false)
	tp, err := f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCLow, 1000, 0xf0000000, 0, 0), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.Less(t, int64(f.dummyStartTSOffset), int64(0))

	// the downtrack has been sending: rtpStats is initialized on the first forwarded packet
	f.rtpStats.Update(mono.UnixNano(), tp.rtp.extSequenceNumber, tp.rtp.extTimestamp, true, 12, 20, 0, false)

	// upstream receiver restart: new stream, new timestamp base for every layer
	f.Restart()
	f.DetermineCodec(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, nil, livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM)
	const newLowStart = uint32(0x20000000)
	now := time.Now()
	f.SetRefSenderReport(0, wrapTestSenderReport(now, newLowStart))
	f.SetRefSenderReport(2, wrapTestSenderReport(now, wrapTestHighTS))

	f.AllocateOptimal([]int32{0}, wrapTestBitrates(0), true, false)
	tp, err = f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCLow, 1, newLowStart, 0, 0), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	lastTS := tp.rtp.extTimestamp

	// one frame later, switch to layer 2: the timestamp must advance by about one frame
	f.AllocateOptimal([]int32{2}, wrapTestBitrates(2), true, false)
	tp, err = f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCHigh, 1, wrapTestHighTS+wrapTestFrame, 0, 2), 2)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	advance := int64(tp.rtp.extTimestamp - lastTS)
	require.Greater(t, advance, int64(0))
	require.Less(t, advance, int64(90000), "timestamp jumped %d ticks (%.1f s) across the switch after restart", advance, float64(advance)/90000)
}

// Stream without a usable reference layer (incomplete sender reports, skipReferenceTS): refTS is
// the last outgoing timestamp, already on the downtrack's timeline. No dummy start offset may be
// computed or applied, otherwise the gap between the last packet and the restart is added again
// on every later switch.
func TestForwarderSkipReferenceTSRestartNoOffset(t *testing.T) {
	f := newWrapTestForwarder(t, true)
	f.rtpStats.SetClockRate(90000)

	f.maybeStart()
	f.AllocateOptimal([]int32{0}, wrapTestBitrates(0), true, false)
	tp, err := f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCLow, 1000, 0xf0000000, 0, 0), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.Zero(t, f.dummyStartTSOffset)
	firstTS := tp.rtp.extTimestamp

	// the last packet went out 5 s ago
	const gap = 5 * time.Second
	f.rtpStats.Update(mono.UnixNano()-gap.Nanoseconds(), tp.rtp.extSequenceNumber, firstTS, true, 12, 20, 0, false)

	// upstream restart, new SSRC: the 5 s gap shows up once, at the restart
	f.Restart()
	f.DetermineCodec(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, nil, livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR)
	f.AllocateOptimal([]int32{0}, wrapTestBitrates(0), true, false)
	tp, err = f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCHigh, 1, 0x20000000, 0, 0), 0)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	require.Zero(t, f.dummyStartTSOffset)
	require.InDelta(t, gap.Seconds()*90000, float64(int64(tp.rtp.extTimestamp-firstTS)), 90000)
	lastTS := tp.rtp.extTimestamp

	// one frame later, switch to another layer: the gap must not be added again
	f.AllocateOptimal([]int32{2}, wrapTestBitrates(2), true, false)
	tp, err = f.GetTranslationParams(wrapTestKeyFrame(t, wrapTestSSRCLow, 1, wrapTestHighTS+wrapTestFrame, 0, 2), 2)
	require.NoError(t, err)
	require.False(t, tp.shouldDrop)
	advance := int64(tp.rtp.extTimestamp - lastTS)
	require.GreaterOrEqual(t, advance, int64(0))
	require.Less(t, advance, int64(90000), "timestamp jumped %d ticks (%.1f s) on the switch after restart", advance, float64(advance)/90000)
}
