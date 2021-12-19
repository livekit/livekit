package sfu

import (
	"reflect"
	"testing"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
)

func disable(f *Forwarder) {
	f.currentLayers = InvalidLayers
	f.targetLayers = InvalidLayers
}

func TestForwarderMute(t *testing.T) {
	f := NewForwarder(testutils.TestOpusCodec, webrtc.RTPCodecTypeAudio)
	require.False(t, f.Muted())
	require.False(t, f.Mute(false)) // no change in mute state
	require.False(t, f.Muted())
	require.True(t, f.Mute(true))
	require.True(t, f.Muted())
	require.True(t, f.Mute(false))
	require.False(t, f.Muted())
}

func TestForwarderLayersAudio(t *testing.T) {
	f := NewForwarder(testutils.TestOpusCodec, webrtc.RTPCodecTypeAudio)

	require.Equal(t, InvalidLayers, f.MaxLayers())

	require.Equal(t, InvalidLayers, f.CurrentLayers())
	require.Equal(t, InvalidLayers, f.TargetLayers())

	changed, layers := f.SetMaxSpatialLayer(1)
	require.False(t, changed)
	require.Equal(t, InvalidLayers, layers)

	changed, layers = f.SetMaxTemporalLayer(1)
	require.False(t, changed)
	require.Equal(t, InvalidLayers, layers)

	require.Equal(t, InvalidLayers, f.MaxLayers())
}

func TestForwarderLayersVideo(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	maxLayers := f.MaxLayers()
	expectedLayers := VideoLayers{
		spatial:  DefaultMaxLayerSpatial,
		temporal: DefaultMaxLayerTemporal,
	}
	require.Equal(t, expectedLayers, maxLayers)

	require.Equal(t, InvalidLayers, f.CurrentLayers())
	require.Equal(t, InvalidLayers, f.TargetLayers())

	changed, layers := f.SetMaxSpatialLayer(DefaultMaxLayerSpatial)
	require.False(t, changed)
	require.Equal(t, InvalidLayers, layers)

	changed, layers = f.SetMaxSpatialLayer(DefaultMaxLayerSpatial - 1)
	require.True(t, changed)
	expectedLayers = VideoLayers{
		spatial:  DefaultMaxLayerSpatial - 1,
		temporal: DefaultMaxLayerTemporal,
	}
	require.Equal(t, expectedLayers, layers)
	require.Equal(t, expectedLayers, f.MaxLayers())

	changed, layers = f.SetMaxTemporalLayer(DefaultMaxLayerTemporal)
	require.False(t, changed)
	require.Equal(t, InvalidLayers, layers)

	changed, layers = f.SetMaxTemporalLayer(DefaultMaxLayerTemporal - 1)
	require.True(t, changed)
	expectedLayers = VideoLayers{
		spatial:  DefaultMaxLayerSpatial - 1,
		temporal: DefaultMaxLayerTemporal - 1,
	}
	require.Equal(t, expectedLayers, layers)
	require.Equal(t, expectedLayers, f.MaxLayers())
}

func TestForwarderGetForwardingStatus(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	// no available layers, should be optimal
	require.Equal(t, ForwardingStatusOptimal, f.GetForwardingStatus())

	// with available layers, should be off
	availableLayers := []uint16{0, 1, 2}
	f.UptrackLayersChange(availableLayers)
	require.Equal(t, ForwardingStatusOff, f.GetForwardingStatus())

	// when muted, should be optimal
	f.Mute(true)
	require.Equal(t, ForwardingStatusOptimal, f.GetForwardingStatus())

	// when target is the max, should be optimal
	f.Mute(false)
	f.targetLayers.spatial = DefaultMaxLayerSpatial
	require.Equal(t, ForwardingStatusOptimal, f.GetForwardingStatus())

	// when target is less than max subscribed and max available, should be partial
	f.targetLayers.spatial = DefaultMaxLayerSpatial - 1
	require.Equal(t, ForwardingStatusPartial, f.GetForwardingStatus())

	// when available layers are lower than max subscribed, optimal as long as target is at max available
	availableLayers = []uint16{0, 1}
	f.UptrackLayersChange(availableLayers)
	require.Equal(t, ForwardingStatusOptimal, f.GetForwardingStatus())
}

func TestForwarderUptrackLayersChange(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	require.Nil(t, f.availableLayers)

	availableLayers := []uint16{0, 1, 2}
	f.UptrackLayersChange(availableLayers)
	require.Equal(t, availableLayers, f.availableLayers)

	availableLayers = []uint16{0, 2}
	f.UptrackLayersChange(availableLayers)
	require.Equal(t, availableLayers, f.availableLayers)

	availableLayers = []uint16{}
	f.UptrackLayersChange(availableLayers)
	require.Equal(t, availableLayers, f.availableLayers)
}

func TestForwarderAllocate(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	emptyBitrates := Bitrates{}
	bitrates := Bitrates{
		{2, 3, 0, 0},
		{4, 0, 0, 5},
		{0, 7, 0, 0},
	}

	// muted should not consume any bandwidth
	f.Mute(true)
	disable(f)
	expectedResult := VideoAllocation{
		state:              VideoAllocationStateMuted,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 0,
		bandwidthDelta:     0,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result := f.Allocate(ChannelCapacityInfinity, bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	// feed dry state
	f.Mute(false)
	f.lastAllocation.state = VideoAllocationStateNone
	disable(f)
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateFeedDry,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 0,
		bandwidthDelta:     0,
		availableLayers:    nil,
		bitrates:           emptyBitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result = f.Allocate(ChannelCapacityInfinity, emptyBitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	// awaiting measurement, i.e. bitrates are not available, but layers available
	f.lastAllocation.state = VideoAllocationStateNone
	disable(f)
	f.UptrackLayersChange([]uint16{0})
	expectedTargetLayers := VideoLayers{
		spatial:  0,
		temporal: DefaultMaxLayerTemporal,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateAwaitingMeasurement,
		change:             VideoStreamingChangeResuming,
		bandwidthRequested: 0,
		bandwidthDelta:     0,
		availableLayers:    []uint16{0},
		bitrates:           emptyBitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  0,
	}
	result = f.Allocate(ChannelCapacityInfinity, emptyBitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
	require.Equal(t, InvalidLayers, f.CurrentLayers())

	// while awaiting measurement, less than infinite channel capacity should pause the stream
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangePausing,
		bandwidthRequested: 0,
		bandwidthDelta:     0,
		availableLayers:    []uint16{0},
		bitrates:           emptyBitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result = f.Allocate(ChannelCapacityInfinity-1, emptyBitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.CurrentLayers())
	require.Equal(t, InvalidLayers, f.TargetLayers())

	// allocate using bitrates and less than infinite channel capacity, but enough for optimal
	expectedTargetLayers = VideoLayers{
		spatial:  2,
		temporal: 1,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateOptimal,
		change:             VideoStreamingChangeResuming,
		bandwidthRequested: bitrates[2][1],
		bandwidthDelta:     bitrates[2][1],
		availableLayers:    []uint16{0},
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  0,
	}
	result = f.Allocate(ChannelCapacityInfinity-1, bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.CurrentLayers())
	require.Equal(t, expectedTargetLayers, f.TargetLayers())

	// give it a bitrate that is less than optimal
	expectedTargetLayers = VideoLayers{
		spatial:  1,
		temporal: 3,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: bitrates[1][3],
		bandwidthDelta:     bitrates[1][3] - bitrates[2][1],
		availableLayers:    []uint16{0},
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  1,
	}
	result = f.Allocate(bitrates[2][1]-1, bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.CurrentLayers())
	require.Equal(t, expectedTargetLayers, f.TargetLayers())

	// give it a bitrate that cannot fit any layer
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangePausing,
		bandwidthRequested: 0,
		bandwidthDelta:     0 - bitrates[1][3],
		availableLayers:    []uint16{0},
		bitrates:           bitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  5,
	}
	result = f.Allocate(bitrates[0][0]-1, bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.CurrentLayers())
	require.Equal(t, InvalidLayers, f.TargetLayers())
}

func TestForwarderProvisionalAllocate(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(bitrates)

	usedBitrate := f.ProvisionalAllocate(bitrates[2][3], VideoLayers{spatial: 0, temporal: 0})
	require.Equal(t, bitrates[0][0], usedBitrate)

	usedBitrate = f.ProvisionalAllocate(bitrates[2][3], VideoLayers{spatial: 1, temporal: 2})
	require.Equal(t, bitrates[1][2], usedBitrate)

	// available not enough to reach (2, 2), allocating at (2, 2) should not succeed
	usedBitrate = f.ProvisionalAllocate(bitrates[2][2]-bitrates[1][2]-1, VideoLayers{spatial: 2, temporal: 2})
	require.Equal(t, int64(0), usedBitrate)

	// committing should set target to (1, 2)
	expectedTargetLayers := VideoLayers{
		spatial:  1,
		temporal: 2,
	}
	expectedResult := VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeResuming,
		bandwidthRequested: bitrates[1][2],
		bandwidthDelta:     bitrates[1][2],
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  5,
	}
	result := f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
}

func TestForwarderProvisionalAllocateMute(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.Mute(true)
	f.ProvisionalAllocatePrepare(bitrates)

	usedBitrate := f.ProvisionalAllocate(bitrates[2][3], VideoLayers{spatial: 0, temporal: 0})
	require.Equal(t, int64(0), usedBitrate)

	usedBitrate = f.ProvisionalAllocate(bitrates[2][3], VideoLayers{spatial: 1, temporal: 2})
	require.Equal(t, int64(0), usedBitrate)

	// committing should set target to InvalidLayers as track is muted
	expectedResult := VideoAllocation{
		state:              VideoAllocationStateMuted,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 0,
		bandwidthDelta:     0,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result := f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.TargetLayers())
}

func TestForwarderProvisionalAllocateGetCooperativeTransition(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 0, 0},
	}

	f.ProvisionalAllocatePrepare(bitrates)

	// from scratch (InvalidLayers) should give back layer (0, 0)
	expectedTransition := VideoTransition{
		from:           InvalidLayers,
		to:             VideoLayers{spatial: 0, temporal: 0},
		bandwidthDelta: 1,
	}
	transition := f.ProvisionalAllocateGetCooperativeTransition()
	require.Equal(t, expectedTransition, transition)

	// committing should set target to (0, 0)
	expectedLayers := VideoLayers{spatial: 0, temporal: 0}
	expectedResult := VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeResuming,
		bandwidthRequested: 1,
		bandwidthDelta:     1,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedLayers,
		distanceToDesired:  9,
	}
	result := f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedLayers, f.TargetLayers())

	// a higher target that is already streaming, just maintain it
	targetLayers := VideoLayers{spatial: 2, temporal: 1}
	f.targetLayers = targetLayers
	f.lastAllocation.bandwidthRequested = 10
	expectedTransition = VideoTransition{
		from:           targetLayers,
		to:             targetLayers,
		bandwidthDelta: 0,
	}
	transition = f.ProvisionalAllocateGetCooperativeTransition()
	require.Equal(t, expectedTransition, transition)

	// committing should set target to (2, 1)
	expectedLayers = VideoLayers{spatial: 2, temporal: 1}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateOptimal,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 10,
		bandwidthDelta:     0,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedLayers,
		distanceToDesired:  0,
	}
	result = f.ProvisionalAllocateCommit()
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedLayers, f.TargetLayers())

	// from a target that has become unavailable, should switch to lower available layer
	targetLayers = VideoLayers{spatial: 2, temporal: 2}
	f.targetLayers = targetLayers
	expectedTransition = VideoTransition{
		from:           targetLayers,
		to:             VideoLayers{spatial: 2, temporal: 1},
		bandwidthDelta: 0,
	}
	transition = f.ProvisionalAllocateGetCooperativeTransition()
	require.Equal(t, expectedTransition, transition)

	f.ProvisionalAllocateCommit()

	// mute
	f.Mute(true)
	f.ProvisionalAllocatePrepare(bitrates)

	// mute should send target to InvalidLayers
	expectedTransition = VideoTransition{
		from:           VideoLayers{spatial: 2, temporal: 1},
		to:             InvalidLayers,
		bandwidthDelta: -10,
	}
	transition = f.ProvisionalAllocateGetCooperativeTransition()
	require.Equal(t, expectedTransition, transition)
}

func TestForwarderProvisionalAllocateGetBestWeightedTransition(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(bitrates)

	f.targetLayers = VideoLayers{spatial: 2, temporal: 2}
	f.lastAllocation.bandwidthRequested = bitrates[2][2]
	expectedTransition := VideoTransition{
		from:           f.targetLayers,
		to:             VideoLayers{spatial: 2, temporal: 0},
		bandwidthDelta: 2,
	}
	transition := f.ProvisionalAllocateGetBestWeightedTransition()
	require.Equal(t, expectedTransition, transition)
}

func TestForwarderFinalizeAllocate(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}
	// FinalizeAllocate should do nothing unless Forwarder allocation state is VideoAllocationStateAwaitingMeasurement
	result := f.FinalizeAllocate(bitrates)
	require.Equal(t, VideoAllocationDefault, result)
	require.Equal(t, VideoAllocationDefault, f.lastAllocation)

	f.lastAllocation.state = VideoAllocationStateMuted
	disable(f)
	expectedResult := VideoAllocation{
		state:              VideoAllocationStateMuted,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 0,
		bandwidthDelta:     0,
		availableLayers:    nil,
		bitrates:           Bitrates{{0, 0, 0, 0}, {0, 0, 0, 0}, {0, 0, 0, 0}},
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result = f.FinalizeAllocate(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	f.lastAllocation.state = VideoAllocationStateAwaitingMeasurement
	disable(f)
	expectedTargetLayers := VideoLayers{
		spatial:  2,
		temporal: 3,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateOptimal,
		change:             VideoStreamingChangeResuming,
		bandwidthRequested: bitrates[2][3],
		bandwidthDelta:     bitrates[2][3],
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  0,
	}
	result = f.FinalizeAllocate(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.CurrentLayers())
	require.Equal(t, expectedTargetLayers, f.TargetLayers())

	// no layers available => feed dry
	f.lastAllocation.state = VideoAllocationStateAwaitingMeasurement
	disable(f)
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateFeedDry,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 0,
		bandwidthDelta:     0 - bitrates[2][3],
		availableLayers:    nil,
		bitrates:           Bitrates{},
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result = f.FinalizeAllocate(Bitrates{})
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	// layers available, but still awaiting measurement
	f.lastAllocation.state = VideoAllocationStateAwaitingMeasurement
	disable(f)
	f.UptrackLayersChange([]uint16{0, 1})
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateAwaitingMeasurement,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 0,
		bandwidthDelta:     -12,
		availableLayers:    nil,
		bitrates:           Bitrates{},
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result = f.FinalizeAllocate(Bitrates{})
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)

	// sparse layers
	bitrates = Bitrates{
		{1, 2, 0, 0},
		{5, 0, 0, 6},
		{0, 0, 0, 0},
	}
	disable(f)
	expectedTargetLayers = VideoLayers{
		spatial:  1,
		temporal: 3,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateOptimal,
		change:             VideoStreamingChangeResuming,
		bandwidthRequested: bitrates[1][3],
		bandwidthDelta:     bitrates[1][3],
		availableLayers:    []uint16{0, 1},
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  0,
	}
	result = f.FinalizeAllocate(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.CurrentLayers())
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
}

func TestForwarderAllocateNextHigher(t *testing.T) {
	f := NewForwarder(testutils.TestOpusCodec, webrtc.RTPCodecTypeAudio)

	emptyBitrates := Bitrates{}
	bitrates := Bitrates{
		{2, 3, 0, 0},
		{4, 0, 0, 5},
		{0, 7, 0, 0},
	}

	result, boosted := f.AllocateNextHigher(bitrates)
	require.Equal(t, VideoAllocationDefault, result) // no layer for audio
	require.False(t, boosted)

	f = NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	// when not in deficient state, does not boost
	f.lastAllocation.state = VideoAllocationStateNone
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, VideoAllocationDefault, result)
	require.False(t, boosted)

	// if layers have not caught up, should not allocate next layer
	f.lastAllocation.state = VideoAllocationStateDeficient
	f.targetLayers.spatial = 0
	expectedResult := VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 0,
		bandwidthDelta:     0,
		availableLayers:    nil,
		bitrates:           emptyBitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.False(t, boosted)
	f.currentLayers.spatial = 0

	f.targetLayers.temporal = 0
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.False(t, boosted)
	f.currentLayers.temporal = 0

	f.lastAllocation.bandwidthRequested = bitrates[0][0]

	// empty bitrates cannot increase layer
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 2,
		bandwidthDelta:     0,
		availableLayers:    nil,
		bitrates:           emptyBitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result, boosted = f.AllocateNextHigher(emptyBitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.False(t, boosted)

	// move from (0, 0) -> (0, 1), i.e. a higher temporal layer is available in the same spatial layer
	expectedTargetLayers := VideoLayers{
		spatial:  0,
		temporal: 1,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 3,
		bandwidthDelta:     1,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  3,
	}
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
	require.True(t, boosted)

	// move from (0, 1) -> (1, 0), i.e. a higher spatial layer is available
	f.currentLayers.temporal = 1
	expectedTargetLayers = VideoLayers{
		spatial:  1,
		temporal: 0,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 4,
		bandwidthDelta:     1,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  2,
	}
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
	require.True(t, boosted)

	// next higher, move from (1, 0) -> (1, 3), still deficient though
	f.currentLayers.spatial = 1
	f.currentLayers.temporal = 0
	expectedTargetLayers = VideoLayers{
		spatial:  1,
		temporal: 3,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 5,
		bandwidthDelta:     1,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  1,
	}
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
	require.True(t, boosted)

	// next higher, move from (1, 3) -> (2, 1), optimal allocation
	f.currentLayers.temporal = 3
	expectedTargetLayers = VideoLayers{
		spatial:  2,
		temporal: 1,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateOptimal,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 7,
		bandwidthDelta:     2,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  0,
	}
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
	require.True(t, boosted)

	// ask again, should return not boosted as there is no room to go higher
	f.currentLayers.spatial = 2
	f.currentLayers.temporal = 1
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
	require.False(t, boosted)

	// turn off everything, allocating next layer should result in streaming lowest layers
	disable(f)
	f.lastAllocation.state = VideoAllocationStateDeficient
	f.lastAllocation.bandwidthRequested = 0

	expectedTargetLayers = VideoLayers{
		spatial:  0,
		temporal: 0,
	}
	expectedResult = VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangeResuming,
		bandwidthRequested: 2,
		bandwidthDelta:     2,
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       expectedTargetLayers,
		distanceToDesired:  4,
	}
	result, boosted = f.AllocateNextHigher(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, expectedTargetLayers, f.TargetLayers())
	require.True(t, boosted)
}

func TestForwarderPause(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(bitrates)
	f.ProvisionalAllocate(bitrates[2][3], VideoLayers{spatial: 0, temporal: 0})
	// should have set target at (0, 0)
	f.ProvisionalAllocateCommit()

	expectedResult := VideoAllocation{
		state:              VideoAllocationStateDeficient,
		change:             VideoStreamingChangePausing,
		bandwidthRequested: 0,
		bandwidthDelta:     0 - bitrates[0][0],
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  12,
	}
	result := f.Pause(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.TargetLayers())
}

func TestForwarderPauseMute(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	bitrates := Bitrates{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
		{9, 10, 11, 12},
	}

	f.ProvisionalAllocatePrepare(bitrates)
	f.ProvisionalAllocate(bitrates[2][3], VideoLayers{spatial: 0, temporal: 0})
	// should have set target at (0, 0)
	f.ProvisionalAllocateCommit()

	f.Mute(true)
	expectedResult := VideoAllocation{
		state:              VideoAllocationStateMuted,
		change:             VideoStreamingChangeNone,
		bandwidthRequested: 0,
		bandwidthDelta:     0 - bitrates[0][0],
		availableLayers:    nil,
		bitrates:           bitrates,
		targetLayers:       InvalidLayers,
		distanceToDesired:  0,
	}
	result := f.Pause(bitrates)
	require.Equal(t, expectedResult, result)
	require.Equal(t, expectedResult, f.lastAllocation)
	require.Equal(t, InvalidLayers, f.TargetLayers())
}

func TestForwarderGetTranslationParamsMuted(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)
	f.Mute(true)

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
	require.Equal(t, expectedTP, *actualTP)
}

func TestForwarderGetTranslationParamsAudio(t *testing.T) {
	f := NewForwarder(testutils.TestOpusCodec, webrtc.RTPCodecTypeAudio)

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, err := testutils.GetTestExtPacket(params)

	// should lock onto the first packet
	expectedTP := TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingContiguous,
			sequenceNumber: 23333,
			timestamp:      0xabcdef,
		},
	}
	actualTP, err := f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))
	require.True(t, f.started)
	require.Equal(t, f.lastSSRC, params.SSRC)

	// send a duplicate, should be dropped
	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// out-of-order packet not in cache should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, err = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		shouldDrop:         true,
		isDroppingRelevant: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// padding only packet in order should be dropped
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23334,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, err = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// in order packet should be forwarded
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23335,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, err = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingContiguous,
			sequenceNumber: 23334,
			timestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// padding only packet after a gap should be forwarded
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23337,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, err = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingGap,
			sequenceNumber: 23336,
			timestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// out-of-order should be forwarded using cache
	params = &testutils.TestExtPacketParams{
		IsHead:         false,
		SequenceNumber: 23336,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, err = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingOutOfOrder,
			sequenceNumber: 23335,
			timestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// switching source should lock onto the new source, but sequence number should be contiguous
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 123,
		Timestamp:      0xfedcba,
		SSRC:           0x87654321,
		PayloadSize:    20,
	}
	extPkt, err = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingContiguous,
			sequenceNumber: 23337,
			timestamp:      0xabcdf0,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))
	require.Equal(t, f.lastSSRC, params.SSRC)
}

func TestForwarderGetTranslationParamsVideo(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 := &buffer.VP8{
		FirstByte:        25,
		PictureIDPresent: 1,
		PictureID:        13467,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        233,
		TIDPresent:       1,
		TID:              1,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           23,
		HeaderSize:       6,
		IsKeyFrame:       false,
	}
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)

	// no target layers, should drop
	expectedTP := TranslationParams{
		shouldDrop: true,
	}
	actualTP, err := f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, *actualTP)

	// although target layer matches, not a key frame, so should drop and ask to send PLI
	f.targetLayers = VideoLayers{
		spatial:  0,
		temporal: 1,
	}
	expectedTP = TranslationParams{
		shouldDrop:    true,
		shouldSendPLI: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.Equal(t, expectedTP, *actualTP)

	// should lock onto packet (target layer and key frame)
	vp8 = &buffer.VP8{
		FirstByte:        25,
		PictureIDPresent: 1,
		PictureID:        13467,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        233,
		TIDPresent:       1,
		TID:              1,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           23,
		HeaderSize:       6,
		IsKeyFrame:       true,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingContiguous,
			sequenceNumber: 23333,
			timestamp:      0xabcdef,
		},
		vp8: &TranslationParamsVP8{
			header: &buffer.VP8{
				FirstByte:        25,
				PictureIDPresent: 1,
				PictureID:        13467,
				MBit:             true,
				TL0PICIDXPresent: 1,
				TL0PICIDX:        233,
				TIDPresent:       1,
				TID:              1,
				Y:                1,
				KEYIDXPresent:    1,
				KEYIDX:           23,
				HeaderSize:       6,
				IsKeyFrame:       true,
			},
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))
	require.True(t, f.started)
	require.Equal(t, f.lastSSRC, params.SSRC)

	// send a duplicate, should be dropped
	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// out-of-order packet not in cache should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedTP = TranslationParams{
		shouldDrop:         true,
		isDroppingRelevant: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// padding only packet in order should be dropped
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
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
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// in order packet should be forwarded
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23335,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingContiguous,
			sequenceNumber: 23334,
			timestamp:      0xabcdef,
		},
		vp8: &TranslationParamsVP8{
			header: &buffer.VP8{
				FirstByte:        25,
				PictureIDPresent: 1,
				PictureID:        13467,
				MBit:             true,
				TL0PICIDXPresent: 1,
				TL0PICIDX:        233,
				TIDPresent:       1,
				TID:              1,
				Y:                1,
				KEYIDXPresent:    1,
				KEYIDX:           23,
				HeaderSize:       6,
				IsKeyFrame:       true,
			},
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// temporal layer higher than target, should be dropped
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23336,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 = &buffer.VP8{
		FirstByte:        25,
		PictureIDPresent: 1,
		PictureID:        13468,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        233,
		TIDPresent:       1,
		TID:              2,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           23,
		HeaderSize:       6,
		IsKeyFrame:       true,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedTP = TranslationParams{
		shouldDrop: true,
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// RTP sequence number and VP8 picture id should be contiguous after dropping higher temporal layer picture
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23337,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 = &buffer.VP8{
		FirstByte:        25,
		PictureIDPresent: 1,
		PictureID:        13469,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        234,
		TIDPresent:       1,
		TID:              0,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           23,
		HeaderSize:       6,
		IsKeyFrame:       false,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingContiguous,
			sequenceNumber: 23335,
			timestamp:      0xabcdef,
		},
		vp8: &TranslationParamsVP8{
			header: &buffer.VP8{
				FirstByte:        25,
				PictureIDPresent: 1,
				PictureID:        13468,
				MBit:             true,
				TL0PICIDXPresent: 1,
				TL0PICIDX:        234,
				TIDPresent:       1,
				TID:              0,
				Y:                1,
				KEYIDXPresent:    1,
				KEYIDX:           23,
				HeaderSize:       6,
				IsKeyFrame:       false,
			},
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// padding only packet after a gap should be forwarded
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23339,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, err = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingGap,
			sequenceNumber: 23337,
			timestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// out-of-order should be forwarded using cache, even if it is padding only
	params = &testutils.TestExtPacketParams{
		IsHead:         false,
		SequenceNumber: 23338,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, err = testutils.GetTestExtPacket(params)

	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingOutOfOrder,
			sequenceNumber: 23336,
			timestamp:      0xabcdef,
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 0)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))

	// switching SSRC (happens for new layer or new track source)
	// should lock onto the new source, but sequence number should be contiguous
	f.targetLayers = VideoLayers{
		spatial:  1,
		temporal: 1,
	}

	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 123,
		Timestamp:      0xfedcba,
		SSRC:           0x87654321,
		PayloadSize:    20,
	}
	vp8 = &buffer.VP8{
		FirstByte:        25,
		PictureIDPresent: 1,
		PictureID:        45,
		MBit:             false,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        12,
		TIDPresent:       1,
		TID:              0,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           30,
		HeaderSize:       5,
		IsKeyFrame:       true,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)

	expectedTP = TranslationParams{
		rtp: &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingContiguous,
			sequenceNumber: 23338,
			timestamp:      0xabcdf0,
		},
		vp8: &TranslationParamsVP8{
			header: &buffer.VP8{
				FirstByte:        25,
				PictureIDPresent: 1,
				PictureID:        13469,
				MBit:             true,
				TL0PICIDXPresent: 1,
				TL0PICIDX:        235,
				TIDPresent:       1,
				TID:              0,
				Y:                1,
				KEYIDXPresent:    1,
				KEYIDX:           24,
				HeaderSize:       6,
				IsKeyFrame:       true,
			},
		},
	}
	actualTP, err = f.GetTranslationParams(extPkt, 1)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expectedTP, *actualTP))
	require.Equal(t, f.lastSSRC, params.SSRC)
}

func TestForwardGetSnTsForPadding(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 := &buffer.VP8{
		FirstByte:        25,
		PictureIDPresent: 1,
		PictureID:        13467,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        233,
		TIDPresent:       1,
		TID:              13,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           23,
		HeaderSize:       6,
		IsKeyFrame:       true,
	}
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)

	f.targetLayers = VideoLayers{
		spatial:  0,
		temporal: 1,
	}
	f.currentLayers = InvalidLayers

	// send it through so that forwarder locks onto stream
	_, _ = f.GetTranslationParams(extPkt, 0)

	// pause stream and get padding, it should still work
	disable(f)

	// should get back frame end needed as the last packet did not have RTP marker set
	snts, err := f.GetSnTsForPadding(5)
	require.NoError(t, err)

	numPadding := 5
	clockRate := uint32(0)
	frameRate := uint32(5)
	var sntsExpected = make([]SnTs, numPadding)
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			sequenceNumber: 23333 + uint16(i) + 1,
			timestamp:      0xabcdef + (uint32(i)*clockRate)/frameRate,
		}
	}
	require.Equal(t, sntsExpected, snts)

	// now that there is a marker, timestamp should jump on first padding when asked again
	snts, err = f.GetSnTsForPadding(numPadding)
	require.NoError(t, err)

	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			sequenceNumber: 23338 + uint16(i) + 1,
			timestamp:      0xabcdef + (uint32(i+1)*clockRate)/frameRate,
		}
	}
	require.Equal(t, sntsExpected, snts)
}

func TestForwardGetSnTsForBlankFrames(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 := &buffer.VP8{
		FirstByte:        25,
		PictureIDPresent: 1,
		PictureID:        13467,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        233,
		TIDPresent:       1,
		TID:              13,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           23,
		HeaderSize:       6,
		IsKeyFrame:       true,
	}
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)

	f.targetLayers = VideoLayers{
		spatial:  0,
		temporal: 1,
	}
	f.currentLayers = InvalidLayers

	// send it through so that forwarder locks onto stream
	_, _ = f.GetTranslationParams(extPkt, 0)

	// should get back frame end needed as the last packet did not have RTP marker set
	snts, frameEndNeeded, err := f.GetSnTsForBlankFrames()
	require.NoError(t, err)
	require.True(t, frameEndNeeded)

	// there should be one more than RTPBlankFramesMax as one would have been allocated to end previous frame
	numPadding := RTPBlankFramesMax + 1
	clockRate := testutils.TestVP8Codec.ClockRate
	frameRate := uint32(30)
	var sntsExpected = make([]SnTs, numPadding)
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			sequenceNumber: 23333 + uint16(i) + 1,
			timestamp:      0xabcdef + (uint32(i)*clockRate)/frameRate,
		}
	}
	require.Equal(t, sntsExpected, snts)

	// now that there is a marker, timestamp should jump on first padding when asked again
	// also number of padding should be RTPBlnkFramesMax
	snts, frameEndNeeded, err = f.GetSnTsForBlankFrames()
	require.NoError(t, err)
	require.False(t, frameEndNeeded)

	numPadding = RTPBlankFramesMax
	sntsExpected = sntsExpected[:numPadding]
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			sequenceNumber: 23340 + uint16(i) + 1,
			timestamp:      0xabcdef + (uint32(i+1)*clockRate)/frameRate,
		}
	}
	require.Equal(t, sntsExpected, snts)
}

func TestForwardGetPaddingVP8(t *testing.T) {
	f := NewForwarder(testutils.TestVP8Codec, webrtc.RTPCodecTypeVideo)

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	vp8 := &buffer.VP8{
		FirstByte:        25,
		PictureIDPresent: 1,
		PictureID:        13467,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        233,
		TIDPresent:       1,
		TID:              13,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           23,
		HeaderSize:       6,
		IsKeyFrame:       true,
	}
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)

	f.targetLayers = VideoLayers{
		spatial:  0,
		temporal: 1,
	}
	f.currentLayers = InvalidLayers

	// send it through so that forwarder locks onto stream
	_, _ = f.GetTranslationParams(extPkt, 0)

	// getting padding with frame end needed, should repeat the last picture id
	expectedVP8 := buffer.VP8{
		FirstByte:        16,
		PictureIDPresent: 1,
		PictureID:        13467,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        233,
		TIDPresent:       1,
		TID:              0,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           23,
		HeaderSize:       6,
		IsKeyFrame:       true,
	}
	blankVP8 := f.GetPaddingVP8(true)
	require.True(t, reflect.DeepEqual(expectedVP8, *blankVP8))

	// getting padding with no frame end needed, should get next picture id
	expectedVP8 = buffer.VP8{
		FirstByte:        16,
		PictureIDPresent: 1,
		PictureID:        13468,
		MBit:             true,
		TL0PICIDXPresent: 1,
		TL0PICIDX:        234,
		TIDPresent:       1,
		TID:              0,
		Y:                1,
		KEYIDXPresent:    1,
		KEYIDX:           24,
		HeaderSize:       6,
		IsKeyFrame:       true,
	}
	blankVP8 = f.GetPaddingVP8(false)
	require.True(t, reflect.DeepEqual(expectedVP8, *blankVP8))
}
