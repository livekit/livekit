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
	"github.com/livekit/protocol/logger"
)

func TestDDForwarder(t *testing.T) {
	raw, err := hex.DecodeString(dependencyDescriptorFixture)
	require.NoError(t, err)
	keyFrameDescriptor := dd.DependencyDescriptor{}
	_, err = (&dd.DependencyDescriptorExtension{Descriptor: &keyFrameDescriptor}).Unmarshal(raw)
	require.NoError(t, err)
	structure := keyFrameDescriptor.AttachedStructure
	require.NotNil(t, structure)

	decodeTargets := []buffer.DependencyDescriptorDecodeTarget{
		{Target: 2, Layer: buffer.VideoLayer{Temporal: 2}},
		{Target: 1, Layer: buffer.VideoLayer{Temporal: 1}},
		{Target: 0, Layer: buffer.VideoLayer{Temporal: 0}},
	}

	packet := func(ssrc uint32, extFrameNum uint64, isKeyFrame bool, restartGeneration int) *buffer.ExtPacket {
		descriptor := keyFrameDescriptor
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
				DecodeTargets:     decodeTargets,
				ExtFrameNum:       extFrameNum,
				RestartGeneration: restartGeneration,
			},
		}
	}

	f := NewDDForwarder(logger.GetLogger())
	forward := func(extPkt *buffer.ExtPacket, temporal int32) *dd.DependencyDescriptor {
		result := VideoLayerSelectorResult{}
		if !f.Forward(extPkt, temporal, &result) {
			return nil
		}
		ddBytes := result.DDBytesSpill
		if ddBytes == nil {
			ddBytes = result.DDBytes[:result.DDBytesLen]
		}
		out := &dd.DependencyDescriptor{}
		_, err := (&dd.DependencyDescriptorExtension{Descriptor: out, Structure: structure}).Unmarshal(ddBytes)
		require.NoError(t, err)
		return out
	}

	// nothing before the first key frame, there is no structure to write it with
	require.Nil(t, forward(packet(1, 999, false, 0), 0))

	// the first key frame keeps the publisher's frame number and carries the structure
	out := forward(packet(1, 1000, true, 0), 0)
	require.Equal(t, uint16(1000), out.FrameNumber)
	require.NotNil(t, out.AttachedStructure)

	// delta frames name the active decode targets, frames dropped in between leave gaps
	out = forward(packet(1, 1001, false, 0), 0)
	require.Equal(t, uint16(1001), out.FrameNumber)
	require.Equal(t, uint32(1<<0), *out.ActiveDecodeTargetsBitmask)
	out = forward(packet(1, 1003, false, 0), 1)
	require.Equal(t, uint16(1003), out.FrameNumber)
	require.Equal(t, uint32(1<<0|1<<1), *out.ActiveDecodeTargetsBitmask)

	// a layer switch: the new layer's lower frame numbers continue after the last one
	out = forward(packet(2, 40, true, 0), 0)
	require.Equal(t, uint16(1004), out.FrameNumber)
	require.NotNil(t, out.AttachedStructure)
	out = forward(packet(2, 41, false, 0), 0)
	require.Equal(t, uint16(1005), out.FrameNumber)

	// a retransmitted key frame packet keeps its number
	out = forward(packet(2, 40, true, 0), 0)
	require.Equal(t, uint16(1004), out.FrameNumber)

	// a late packet of the previous layer is not described
	require.Nil(t, forward(packet(1, 1004, false, 0), 0))

	// a restarted stream starts its frame numbers over, they still continue
	out = forward(packet(2, 5, true, 1), 0)
	require.Equal(t, uint16(1006), out.FrameNumber)
	out = forward(packet(2, 6, false, 1), 0)
	require.Equal(t, uint16(1007), out.FrameNumber)
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
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{Descriptor: &keyFrameDescriptor, DecodeTargets: decodeTargets, ExtFrameNum: 100},
	}
	deltaDescriptor := keyFrameDescriptor
	deltaDescriptor.AttachedStructure = nil
	deltaDescriptor.ActiveDecodeTargetsBitmask = nil
	delta := &buffer.ExtPacket{
		Packet:               &rtp.Packet{Header: rtp.Header{SSRC: 1}},
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{Descriptor: &deltaDescriptor, DecodeTargets: decodeTargets, ExtFrameNum: 101},
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
