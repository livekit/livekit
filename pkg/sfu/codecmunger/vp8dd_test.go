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

package codecmunger

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
	"github.com/livekit/protocol/logger"
)

func vp8DDPacket(t *testing.T, frameNum uint64, tid int) *buffer.ExtPacket {
	extPkt, err := testutils.GetTestExtPacketVP8DD(
		&testutils.TestExtPacketParams{SequenceNumber: uint16(frameNum), PayloadSize: 20},
		&testutils.TestDDParams{
			FirstPacketInFrame: true,
			ExtFrameNum:        frameNum,
			TemporalID:         tid,
			DecodeTargets:      testutils.TestVP8DecodeTargets,
		},
	)
	require.NoError(t, err)
	return extPkt
}

func TestVP8DDUpdateAndGet(t *testing.T) {
	t.Run("filters by temporal layer, header unchanged", func(t *testing.T) {
		v := NewVP8DD(logger.GetLogger())
		inputSize, _, n, err := v.UpdateAndGet(vp8DDPacket(t, 0, 0), false, false, 0)
		require.NoError(t, err)
		require.Zero(t, inputSize)
		require.Zero(t, n)

		_, _, _, err = v.UpdateAndGet(vp8DDPacket(t, 1, 2), false, false, 0)
		require.ErrorIs(t, err, ErrFilteredVP8TemporalLayer)
	})

	t.Run("a gap does not exempt a filtered layer", func(t *testing.T) {
		v := NewVP8DD(logger.GetLogger())
		_, _, _, err := v.UpdateAndGet(vp8DDPacket(t, 0, 0), false, false, 0)
		require.NoError(t, err)
		for frameNum := uint64(2); frameNum < 6; frameNum++ {
			_, _, _, err = v.UpdateAndGet(vp8DDPacket(t, frameNum, 1), false, true, 0)
			require.ErrorIs(t, err, ErrFilteredVP8TemporalLayer)
		}
	})

	t.Run("every packet of a frame gets the decision of its first packet", func(t *testing.T) {
		v := NewVP8DD(logger.GetLogger())
		_, _, _, err := v.UpdateAndGet(vp8DDPacket(t, 1, 2), false, false, 0)
		require.ErrorIs(t, err, ErrFilteredVP8TemporalLayer)
		_, _, _, err = v.UpdateAndGet(vp8DDPacket(t, 2, 1), false, false, 1)
		require.NoError(t, err)

		// the layer changed in between, late packets keep the decision of their frame
		_, _, _, err = v.UpdateAndGet(vp8DDPacket(t, 1, 2), true, false, 2)
		require.ErrorIs(t, err, ErrFilteredVP8OutOfOrder)
		_, _, _, err = v.UpdateAndGet(vp8DDPacket(t, 2, 1), true, false, 0)
		require.NoError(t, err)
	})

	t.Run("late packet of a frame older than the window", func(t *testing.T) {
		v := NewVP8DD(logger.GetLogger())
		_, _, _, err := v.UpdateAndGet(vp8DDPacket(t, 100, 0), false, false, 2)
		require.NoError(t, err)
		_, _, _, err = v.UpdateAndGet(vp8DDPacket(t, 100-vp8DDFrameWindow, 0), true, false, 2)
		require.ErrorIs(t, err, ErrFilteredVP8OutOfOrder)
	})

	t.Run("layer switch forgets decisions", func(t *testing.T) {
		v := NewVP8DD(logger.GetLogger())
		_, _, _, err := v.UpdateAndGet(vp8DDPacket(t, 1, 2), false, false, 0)
		require.ErrorIs(t, err, ErrFilteredVP8TemporalLayer)
		v.UpdateOffsets(vp8DDPacket(t, 1, 0))
		_, _, _, err = v.UpdateAndGet(vp8DDPacket(t, 1, 2), false, false, 2)
		require.NoError(t, err)
	})

	t.Run("padding", func(t *testing.T) {
		v := NewVP8DD(logger.GetLogger())
		hdr, err := v.UpdateAndGetPadding(true)
		require.NoError(t, err)
		require.Equal(t, []byte{0x10}, hdr)
	})
}
