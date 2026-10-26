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
	"encoding/hex"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// ddForwarderTest builds packets with the L3T3 fixture descriptor and decodes what a
// DDForwarder writes
type ddForwarderTest struct {
	t                  *testing.T
	keyFrameDescriptor dd.DependencyDescriptor
	structure          *dd.FrameDependencyStructure
	decodeTargets      []buffer.DependencyDescriptorDecodeTarget
}

func newDDForwarderTest(t *testing.T) *ddForwarderTest {
	raw, err := hex.DecodeString(dependencyDescriptorFixture)
	require.NoError(t, err)
	keyFrameDescriptor := dd.DependencyDescriptor{}
	_, err = (&dd.DependencyDescriptorExtension{Descriptor: &keyFrameDescriptor}).Unmarshal(raw)
	require.NoError(t, err)
	require.NotNil(t, keyFrameDescriptor.AttachedStructure)

	return &ddForwarderTest{
		t:                  t,
		keyFrameDescriptor: keyFrameDescriptor,
		structure:          keyFrameDescriptor.AttachedStructure,
		decodeTargets: []buffer.DependencyDescriptorDecodeTarget{
			{Target: 2, Layer: buffer.VideoLayer{Temporal: 2}},
			{Target: 1, Layer: buffer.VideoLayer{Temporal: 1}},
			{Target: 0, Layer: buffer.VideoLayer{Temporal: 0}},
		},
	}
}

func (d *ddForwarderTest) packet(ssrc uint32, extFrameNum uint64, isKeyFrame bool, restartGeneration int) *buffer.ExtPacket {
	descriptor := d.keyFrameDescriptor
	descriptor.FrameNumber = uint16(extFrameNum)
	if !isKeyFrame {
		descriptor.AttachedStructure = nil
		descriptor.ActiveDecodeTargetsBitmask = nil
	}
	return &buffer.ExtPacket{
		Packet:     &rtp.Packet{Header: rtp.Header{SSRC: ssrc}},
		IsKeyFrame: isKeyFrame,
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{
			Descriptor:        &descriptor,
			DecodeTargets:     d.decodeTargets,
			ExtFrameNum:       extFrameNum,
			RestartGeneration: restartGeneration,
			// all decode targets active, as the parser reports after a structure
			ActiveDecodeTargetsMask: 1<<len(d.decodeTargets) - 1,
		},
	}
}

// forward returns the decoded descriptor, nil when f writes none
func (d *ddForwarderTest) forward(f *DDForwarder, extPkt *buffer.ExtPacket, temporal int32) *dd.DependencyDescriptor {
	result := VideoLayerSelectorResult{}
	if !f.Forward(extPkt, temporal, &result) {
		return nil
	}
	ddBytes := result.DDBytesSpill
	if ddBytes == nil {
		ddBytes = result.DDBytes[:result.DDBytesLen]
	}
	out := &dd.DependencyDescriptor{}
	_, err := (&dd.DependencyDescriptorExtension{Descriptor: out, Structure: d.structure}).Unmarshal(ddBytes)
	require.NoError(d.t, err)
	return out
}

func TestDDForwarder(t *testing.T) {
	d := newDDForwarderTest(t)
	f := NewDDForwarder(logger.GetLogger())

	// nothing before the first key frame, there is no structure to write it with
	require.Nil(t, d.forward(f, d.packet(1, 999, false, 0), 0))

	// the first key frame keeps the publisher's frame number and carries the structure,
	// with the decode targets the subscriber gets, not all of them
	out := d.forward(f, d.packet(1, 1000, true, 0), 0)
	require.Equal(t, uint16(1000), out.FrameNumber)
	require.NotNil(t, out.AttachedStructure)
	require.Equal(t, uint32(1<<0), *out.ActiveDecodeTargetsBitmask)

	// delta frames name the active decode targets, frames dropped in between leave gaps
	out = d.forward(f, d.packet(1, 1001, false, 0), 0)
	require.Equal(t, uint16(1001), out.FrameNumber)
	require.Equal(t, uint32(1<<0), *out.ActiveDecodeTargetsBitmask)
	out = d.forward(f, d.packet(1, 1003, false, 0), 1)
	require.Equal(t, uint16(1003), out.FrameNumber)
	require.Equal(t, uint32(1<<0|1<<1), *out.ActiveDecodeTargetsBitmask)

	// a layer switch: the new layer's lower frame numbers continue after the last one
	out = d.forward(f, d.packet(2, 40, true, 0), 0)
	require.Equal(t, uint16(1004), out.FrameNumber)
	require.NotNil(t, out.AttachedStructure)
	out = d.forward(f, d.packet(2, 41, false, 0), 0)
	require.Equal(t, uint16(1005), out.FrameNumber)

	// a retransmitted key frame packet keeps its number
	out = d.forward(f, d.packet(2, 40, true, 0), 0)
	require.Equal(t, uint16(1004), out.FrameNumber)

	// a late packet of the previous layer is not described
	require.Nil(t, d.forward(f, d.packet(1, 1004, false, 0), 0))

	// a restarted stream starts its frame numbers over, they still continue
	out = d.forward(f, d.packet(2, 5, true, 1), 0)
	require.Equal(t, uint16(1006), out.FrameNumber)
	out = d.forward(f, d.packet(2, 6, false, 1), 0)
	require.Equal(t, uint16(1007), out.FrameNumber)
}

func TestDDForwarderSharedFrameCounter(t *testing.T) {
	// libwebrtc numbers the frames of all simulcast layers with one counter
	d := newDDForwarderTest(t)
	f := NewDDForwarder(logger.GetLogger())

	require.Equal(t, uint16(100), d.forward(f, d.packet(1, 100, true, 0), 0).FrameNumber)
	require.Equal(t, uint16(102), d.forward(f, d.packet(1, 102, false, 0), 0).FrameNumber)

	// the new layer's key frame was encoded before the last frame sent, it moves up
	require.Equal(t, uint16(103), d.forward(f, d.packet(2, 101, true, 0), 0).FrameNumber)
	require.Equal(t, uint16(106), d.forward(f, d.packet(2, 104, false, 0), 0).FrameNumber)

	// a key frame ahead of the last frame sent keeps the current offset
	require.Equal(t, uint16(112), d.forward(f, d.packet(1, 110, true, 0), 0).FrameNumber)
}

func TestDDForwarderFromOther(t *testing.T) {
	d := newDDForwarderTest(t)
	f := NewDDForwarder(logger.GetLogger())
	require.Equal(t, uint16(100), d.forward(f, d.packet(1, 100, true, 0), 0).FrameNumber)
	require.Equal(t, uint16(103), d.forward(f, d.packet(1, 103, false, 0), 0).FrameNumber)

	// a replacement on the same downtrack, a restarted upstream receiver for example, gets
	// a key frame behind the last frame sent: it continues after the last frame sent
	f = NewDDForwarderFromOther(f, logger.GetLogger())
	require.Nil(t, d.forward(f, d.packet(1, 101, false, 0), 0))
	require.Equal(t, uint16(104), d.forward(f, d.packet(1, 101, true, 0), 0).FrameNumber)
	require.Equal(t, uint16(105), d.forward(f, d.packet(1, 102, false, 0), 0).FrameNumber)

	// a replacement of nothing keeps the publisher's frame numbers
	f = NewDDForwarderFromOther(nil, logger.GetLogger())
	require.Equal(t, uint16(50), d.forward(f, d.packet(2, 50, true, 0), 0).FrameNumber)
}

func TestDDForwarderRestartOnDeltaFrame(t *testing.T) {
	d := newDDForwarderTest(t)
	f := NewDDForwarder(logger.GetLogger())
	require.Equal(t, uint16(1000), d.forward(f, d.packet(1, 1000, true, 0), 0).FrameNumber)
	require.Equal(t, uint16(1001), d.forward(f, d.packet(1, 1001, false, 0), 0).FrameNumber)

	// the restarted stream's first packet is a delta frame: it has no structure to be
	// written with, but its numbers already continue
	require.Nil(t, d.forward(f, d.packet(1, 3, false, 1), 0))
	require.Equal(t, uint16(1003), d.forward(f, d.packet(1, 4, true, 1), 0).FrameNumber)
	require.Equal(t, uint16(1004), d.forward(f, d.packet(1, 5, false, 1), 0).FrameNumber)
}

func TestDDForwarderPublisherActiveDecodeTargets(t *testing.T) {
	d := newDDForwarderTest(t)
	f := NewDDForwarder(logger.GetLogger())
	require.Equal(t, uint32(1<<0|1<<1|1<<2), *d.forward(f, d.packet(1, 100, true, 0), 2).ActiveDecodeTargetsBitmask)

	// the publisher turned off its upper decode targets on a packet that was not forwarded,
	// the next forwarded packet still reports it
	delta := d.packet(1, 102, false, 0)
	delta.DependencyDescriptor.ActiveDecodeTargetsMask = 1 << 0
	require.Equal(t, uint32(1<<0), *d.forward(f, delta, 2).ActiveDecodeTargetsBitmask)

	// and turned them on again
	require.Equal(t, uint32(1<<0|1<<1|1<<2), *d.forward(f, d.packet(1, 103, true, 0), 2).ActiveDecodeTargetsBitmask)
}

func TestDDForwarderState(t *testing.T) {
	d := newDDForwarderTest(t)
	f := NewDDForwarder(logger.GetLogger())
	require.Nil(t, f.GetState())
	require.Equal(t, uint16(1000), d.forward(f, d.packet(1, 1000, true, 0), 0).FrameNumber)
	require.Equal(t, uint16(1003), d.forward(f, d.packet(1, 1003, false, 0), 0).FrameNumber)
	state := f.GetState()
	require.Equal(t, uint64(1003), state.ExtLastFrameNumber)

	// a migrated subscriber whose new node's key frame is behind continues after the last
	// number sent
	f = NewDDForwarder(logger.GetLogger())
	f.SeedState(state)
	require.Nil(t, d.forward(f, d.packet(1, 999, false, 0), 0))
	require.Equal(t, uint16(1004), d.forward(f, d.packet(1, 1001, true, 0), 0).FrameNumber)

	// the old node had wrapped its extended numbers, the new node starts from the 16 bit
	// value: the 16 bit numbers still continue
	f = NewDDForwarder(logger.GetLogger())
	f.SeedState(&livekit.DependencyDescriptorForwarderState{ExtLastFrameNumber: 1<<16 + 1003})
	require.Equal(t, uint16(1004), d.forward(f, d.packet(1, 1001, true, 0), 0).FrameNumber)
}

func TestDDForwarderAllocations(t *testing.T) {
	raw, err := hex.DecodeString(dependencyDescriptorFixture)
	require.NoError(t, err)
	keyFrameDescriptor := dd.DependencyDescriptor{}
	_, err = (&dd.DependencyDescriptorExtension{Descriptor: &keyFrameDescriptor}).Unmarshal(raw)
	require.NoError(t, err)

	decodeTargets := []buffer.DependencyDescriptorDecodeTarget{{Target: 0, Layer: buffer.VideoLayer{Temporal: 0}}}
	keyFrame := &buffer.ExtPacket{
		Packet:               &rtp.Packet{Header: rtp.Header{SSRC: 1}},
		IsKeyFrame:           true,
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{Descriptor: &keyFrameDescriptor, DecodeTargets: decodeTargets, ExtFrameNum: 100, ActiveDecodeTargetsMask: 1},
	}
	deltaDescriptor := keyFrameDescriptor
	deltaDescriptor.AttachedStructure = nil
	deltaDescriptor.ActiveDecodeTargetsBitmask = nil
	delta := &buffer.ExtPacket{
		Packet:               &rtp.Packet{Header: rtp.Header{SSRC: 1}},
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{Descriptor: &deltaDescriptor, DecodeTargets: decodeTargets, ExtFrameNum: 101, ActiveDecodeTargetsMask: 1},
	}

	f := NewDDForwarder(logger.GetLogger())
	result := VideoLayerSelectorResult{}
	require.True(t, f.Forward(keyFrame, 0, &result))

	// a delta frame's descriptor fits inline
	allocs := testing.AllocsPerRun(100, func() {
		delta.DependencyDescriptor.ExtFrameNum++
		result = VideoLayerSelectorResult{}
		if !f.Forward(delta, 0, &result) {
			t.Fatal("not forwarded")
		}
	})
	require.Zero(t, allocs)
}
