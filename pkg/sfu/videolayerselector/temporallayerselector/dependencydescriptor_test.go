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

package temporallayerselector

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
	"github.com/livekit/protocol/logger"
)

type vp8DDFrame struct {
	tid        int
	dtis       string
	frameDiffs []int
	keyFrame   bool
}

// libwebrtc's three layer VP8 pattern: T0 refreshes "last", T1 refreshes "golden", and
// frame diffs point at the frames in those buffers
var vp8DDPattern = []vp8DDFrame{
	{tid: 0, dtis: "SSS", frameDiffs: []int{4}},
	{tid: 2, dtis: "--D", frameDiffs: []int{1}},
	{tid: 1, dtis: "-SS", frameDiffs: []int{2}},
	{tid: 2, dtis: "--D", frameDiffs: []int{1, 3}},
	{tid: 0, dtis: "SRR", frameDiffs: []int{4}},
	{tid: 2, dtis: "--D", frameDiffs: []int{1, 3}},
	{tid: 1, dtis: "-DS", frameDiffs: []int{2, 4}},
	{tid: 2, dtis: "--D", frameDiffs: []int{1, 3}},
}

func vp8DDPacket(t *testing.T, frameNum uint64, f vp8DDFrame, first bool, marker bool) *buffer.ExtPacket {
	extPkt, err := testutils.GetTestExtPacketVP8DD(
		&testutils.TestExtPacketParams{SequenceNumber: uint16(frameNum), PayloadSize: 20, Marker: marker, IsKeyFrame: f.keyFrame},
		&testutils.TestDDParams{
			FirstPacketInFrame: first,
			ExtFrameNum:        frameNum,
			TemporalID:         f.tid,
			DTIs:               f.dtis,
			FrameDiffs:         f.frameDiffs,
			DecodeTargets:      testutils.TestVP8DecodeTargets,
		},
	)
	require.NoError(t, err)
	return extPkt
}

// runDependencyDescriptor sends one packet per frame: a key frame, the pattern at the start target until
// frame switchAt, then at target 2. It returns the frame number of the up-switch.
func runDependencyDescriptor(t *testing.T, startTarget int32, switchAt uint64) (uint64, bool) {
	v := NewDependencyDescriptor(logger.GetLogger())
	current := int32(0)
	for frameNum := uint64(0); frameNum < 40; frameNum++ {
		f := vp8DDPattern[frameNum%uint64(len(vp8DDPattern))]
		if frameNum == 0 {
			f = vp8DDFrame{tid: 0, dtis: "SSS", keyFrame: true}
		}
		target := startTarget
		if frameNum >= switchAt {
			target = 2
		}
		this, next := v.Select(vp8DDPacket(t, frameNum, f, true, true), current, target)
		if frameNum >= switchAt && next == 2 {
			require.Equal(t, int32(2), this)
			return frameNum, true
		}
		current = next
	}
	return 0, false
}

func TestDependencyDescriptorSelect(t *testing.T) {
	t.Run("from TL0, up-switch at the next T0 switch point", func(t *testing.T) {
		// frame 14 is the "-DS" T1 frame, it references T1 frame 10 that was dropped
		frameNum, ok := runDependencyDescriptor(t, 0, 12)
		require.True(t, ok)
		require.Equal(t, uint64(16), frameNum)

		// frame 10 is the "-SS" T1 frame, it references only T0
		frameNum, ok = runDependencyDescriptor(t, 0, 10)
		require.True(t, ok)
		require.Equal(t, uint64(10), frameNum)
	})

	t.Run("from TL1, the -DS frame is a switch point", func(t *testing.T) {
		frameNum, ok := runDependencyDescriptor(t, 1, 14)
		require.True(t, ok)
		require.Equal(t, uint64(14), frameNum)
	})

	t.Run("switch point that references a dropped frame", func(t *testing.T) {
		// libwebrtc L1T4: a T2 frame that is a switch point for TL2 but references the
		// previous T2 frame
		v := NewDependencyDescriptor(logger.GetLogger())
		_, current := v.Select(vp8DDPacket(t, 0, vp8DDFrame{tid: 0, dtis: "SSS", keyFrame: true}, true, true), 1, 1)
		_, current = v.Select(vp8DDPacket(t, 1, vp8DDFrame{tid: 2, dtis: "--D", frameDiffs: []int{1}}, true, true), current, 1)
		this, next := v.Select(vp8DDPacket(t, 2, vp8DDFrame{tid: 2, dtis: "--S", frameDiffs: []int{1}}, true, true), current, 2)
		require.Equal(t, int32(1), this)
		require.Equal(t, int32(1), next)
	})

	t.Run("reference lost upstream", func(t *testing.T) {
		v := NewDependencyDescriptor(logger.GetLogger())
		_, current := v.Select(vp8DDPacket(t, 0, vp8DDFrame{tid: 0, dtis: "SSS", keyFrame: true}, true, true), 0, 0)
		// frame 1 never arrives
		this, next := v.Select(vp8DDPacket(t, 2, vp8DDFrame{tid: 1, dtis: "-SS", frameDiffs: []int{1}}, true, true), current, 2)
		require.Equal(t, int32(0), this)
		require.Equal(t, int32(0), next)
	})

	t.Run("up-switch only on the first packet of a frame, in order", func(t *testing.T) {
		v := NewDependencyDescriptor(logger.GetLogger())
		_, current := v.Select(vp8DDPacket(t, 0, vp8DDFrame{tid: 0, dtis: "SSS", keyFrame: true}, true, true), 0, 0)
		f := vp8DDFrame{tid: 1, dtis: "-SS", frameDiffs: []int{1}}

		_, next := v.Select(vp8DDPacket(t, 1, f, false, true), current, 2)
		require.Equal(t, int32(0), next, "second packet")

		late := vp8DDPacket(t, 1, f, true, true)
		late.IsOutOfOrder = true
		_, next = v.Select(late, current, 2)
		require.Equal(t, int32(0), next, "late first packet")

		this, next := v.Select(vp8DDPacket(t, 1, f, true, true), current, 2)
		require.Equal(t, int32(2), this)
		require.Equal(t, int32(2), next)
	})

	t.Run("late key frame packet of a stream restart", func(t *testing.T) {
		v := NewDependencyDescriptor(logger.GetLogger())
		_, current := v.Select(vp8DDPacket(t, 100, vp8DDFrame{tid: 0, dtis: "SSS", keyFrame: true}, true, true), 0, 0)

		// the restart moves the frame numbers back, and its key frame arrives out of order
		keyFrame := vp8DDPacket(t, 5, vp8DDFrame{tid: 0, dtis: "SSS", keyFrame: true}, true, true)
		keyFrame.IsOutOfOrder = true
		_, current = v.Select(keyFrame, current, 0)

		this, next := v.Select(vp8DDPacket(t, 6, vp8DDFrame{tid: 1, dtis: "-SS", frameDiffs: []int{1}}, true, true), current, 2)
		require.Equal(t, int32(2), this)
		require.Equal(t, int32(2), next)
	})

	t.Run("key frame", func(t *testing.T) {
		v := NewDependencyDescriptor(logger.GetLogger())
		this, next := v.Select(vp8DDPacket(t, 0, vp8DDFrame{tid: 0, dtis: "SSS", keyFrame: true}, true, false), 0, 2)
		require.Equal(t, int32(2), this)
		require.Equal(t, int32(2), next)
	})

	t.Run("no dependency descriptor", func(t *testing.T) {
		v := NewDependencyDescriptor(logger.GetLogger())
		extPkt := vp8DDPacket(t, 0, vp8DDFrame{tid: 0, dtis: "SSS", keyFrame: true}, true, true)
		extPkt.DependencyDescriptor = nil
		this, next := v.Select(extPkt, 0, 2)
		require.Equal(t, int32(0), this)
		require.Equal(t, int32(0), next)
	})

	t.Run("down-switch at the end of a frame", func(t *testing.T) {
		v := NewDependencyDescriptor(logger.GetLogger())
		f := vp8DDFrame{tid: 2, dtis: "--D", frameDiffs: []int{1}}
		this, next := v.Select(vp8DDPacket(t, 1, f, true, false), 2, 0)
		require.Equal(t, int32(2), this)
		require.Equal(t, int32(2), next)
		this, next = v.Select(vp8DDPacket(t, 1, f, false, true), 2, 0)
		require.Equal(t, int32(2), this)
		require.Equal(t, int32(0), next)
	})
}
