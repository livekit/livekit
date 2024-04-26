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

package codecmunger

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
)

func compare(expected *VP8, actual *VP8) bool {
	return reflect.DeepEqual(expected.pictureIdWrapHandler, actual.pictureIdWrapHandler) &&
		expected.extLastPictureId == actual.extLastPictureId &&
		expected.pictureIdOffset == actual.pictureIdOffset &&
		expected.pictureIdUsed == actual.pictureIdUsed &&
		expected.lastTl0PicIdx == actual.lastTl0PicIdx &&
		expected.tl0PicIdxOffset == actual.tl0PicIdxOffset &&
		expected.tl0PicIdxUsed == actual.tl0PicIdxUsed &&
		expected.tidUsed == actual.tidUsed &&
		expected.lastKeyIdx == actual.lastKeyIdx &&
		expected.keyIdxOffset == actual.keyIdxOffset &&
		expected.keyIdxUsed == actual.keyIdxUsed
}

func newVP8() *VP8 {
	return NewVP8(logger.GetLogger())
}

func TestSetLast(t *testing.T) {
	v := newVP8()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
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
	extPkt, err := testutils.GetTestExtPacketVP8(params, vp8)
	require.NoError(t, err)
	require.NotNil(t, extPkt)

	expectedVP8 := VP8{
		pictureIdWrapHandler: VP8PictureIdWrapHandler{
			maxPictureId: 13466,
			maxMBit:      true,
			totalWrap:    0,
			lastWrap:     0,
		},
		extLastPictureId: 13467,
		pictureIdOffset:  0,
		pictureIdUsed:    true,
		lastTl0PicIdx:    233,
		tl0PicIdxOffset:  0,
		tl0PicIdxUsed:    true,
		tidUsed:          true,
		lastKeyIdx:       23,
		keyIdxOffset:     0,
		keyIdxUsed:       true,
	}

	v.SetLast(extPkt)
	require.True(t, compare(&expectedVP8, v))
}

func TestUpdateOffsets(t *testing.T) {
	v := newVP8()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
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
	v.SetLast(extPkt)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 56789,
		Timestamp:      0xabcdef,
		SSRC:           0x87654321,
	}
	vp8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  345,
		L:          true,
		TL0PICIDX:  12,
		T:          true,
		TID:        13,
		Y:          true,
		K:          true,
		KEYIDX:     4,
		HeaderSize: 6,
		IsKeyFrame: true,
	}
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)
	v.UpdateOffsets(extPkt)

	expectedVP8 := VP8{
		pictureIdWrapHandler: VP8PictureIdWrapHandler{
			maxPictureId: 344,
			maxMBit:      true,
			totalWrap:    0,
			lastWrap:     0,
		},
		extLastPictureId: 13467,
		pictureIdOffset:  345 - 13467 - 1,
		pictureIdUsed:    true,
		lastTl0PicIdx:    233,
		tl0PicIdxOffset:  (12 - 233 - 1) & 0xff,
		tl0PicIdxUsed:    true,
		tidUsed:          true,
		lastKeyIdx:       23,
		keyIdxOffset:     (4 - 23 - 1) & 0x1f,
		keyIdxUsed:       true,
	}
	require.True(t, compare(&expectedVP8, v))
}

func TestOutOfOrderPictureId(t *testing.T) {
	v := newVP8()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	vp8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
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
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)
	v.SetLast(extPkt)
	v.UpdateAndGet(extPkt, false, false, 2)

	// out-of-order sequence number not in the missing picture id cache
	vp8.PictureID = 13466
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)

	nIn, buf, err := v.UpdateAndGet(extPkt, true, false, 2)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOutOfOrderVP8PictureIdCacheMiss)
	require.Equal(t, 0, nIn)
	require.Nil(t, buf)

	// create a hole in picture id
	vp8.PictureID = 13469
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)

	expectedVP8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13469,
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
	marshalledVP8, err := expectedVP8.Marshal()
	require.NoError(t, err)
	nIn, buf, err = v.UpdateAndGet(extPkt, false, true, 2)
	require.NoError(t, err)
	require.Equal(t, 6, nIn)
	require.Equal(t, marshalledVP8, buf)

	// all three, the last, the current and the in-between should have been added to missing picture id cache
	value, ok := v.PictureIdOffset(13467)
	require.True(t, ok)
	require.EqualValues(t, 0, value)

	value, ok = v.PictureIdOffset(13468)
	require.True(t, ok)
	require.EqualValues(t, 0, value)

	value, ok = v.PictureIdOffset(13469)
	require.True(t, ok)
	require.EqualValues(t, 0, value)

	// out-of-order sequence number should be in the missing picture id cache
	vp8.PictureID = 13468
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
	nIn, buf, err = v.UpdateAndGet(extPkt, true, false, 2)
	require.NoError(t, err)
	require.Equal(t, 6, nIn)
	require.Equal(t, marshalledVP8, buf)
}

func TestTemporalLayerFiltering(t *testing.T) {
	v := newVP8()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	vp8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
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
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)
	v.SetLast(extPkt)

	// translate
	nIn, buf, err := v.UpdateAndGet(extPkt, false, false, 0)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrFilteredVP8TemporalLayer)
	require.Equal(t, 0, nIn)
	require.Nil(t, buf)
	dropped, _ := v.droppedPictureIds.Get(13467)
	require.True(t, dropped)
	require.EqualValues(t, 1, v.pictureIdOffset)

	// another packet with the same picture id.
	// It should be dropped, but offset should not be updated.
	params.SequenceNumber = 23334
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)

	nIn, buf, err = v.UpdateAndGet(extPkt, false, false, 0)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrFilteredVP8TemporalLayer)
	require.Equal(t, 0, nIn)
	require.Nil(t, buf)
	dropped, _ = v.droppedPictureIds.Get(13467)
	require.True(t, dropped)
	require.EqualValues(t, 1, v.pictureIdOffset)

	// another packet with the same picture id, but a gap in sequence number.
	// It should be dropped, but offset should not be updated.
	params.SequenceNumber = 23337
	extPkt, _ = testutils.GetTestExtPacketVP8(params, vp8)

	nIn, buf, err = v.UpdateAndGet(extPkt, false, false, 0)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrFilteredVP8TemporalLayer)
	require.Equal(t, 0, nIn)
	require.Nil(t, buf)
	dropped, _ = v.droppedPictureIds.Get(13467)
	require.True(t, dropped)
	require.EqualValues(t, 1, v.pictureIdOffset)
}

func TestGapInSequenceNumberSamePicture(t *testing.T) {
	v := newVP8()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 65533,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    33,
	}
	vp8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
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
	extPkt, _ := testutils.GetTestExtPacketVP8(params, vp8)
	v.SetLast(extPkt)

	expectedVP8 := &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
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
	marshalledVP8, err := expectedVP8.Marshal()
	require.NoError(t, err)
	nIn, buf, err := v.UpdateAndGet(extPkt, false, false, 2)
	require.NoError(t, err)
	require.Equal(t, 6, nIn)
	require.Equal(t, marshalledVP8, buf)

	// telling there is a gap in sequence number will add pictures to missing picture cache
	expectedVP8 = &buffer.VP8{
		FirstByte:  25,
		I:          true,
		M:          true,
		PictureID:  13467,
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
	nIn, buf, err = v.UpdateAndGet(extPkt, false, true, 2)
	require.NoError(t, err)
	require.Equal(t, 6, nIn)
	require.Equal(t, marshalledVP8, buf)

	value, ok := v.PictureIdOffset(13467)
	require.True(t, ok)
	require.EqualValues(t, 0, value)
}

func TestUpdateAndGetPadding(t *testing.T) {
	v := newVP8()

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

	v.SetLast(extPkt)

	// getting padding with repeat of last picture
	buf, err := v.UpdateAndGetPadding(false)
	require.NoError(t, err)
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
	marshalledVP8, err := expectedVP8.Marshal()
	require.NoError(t, err)
	require.Equal(t, marshalledVP8, buf)

	// getting padding with new picture
	buf, err = v.UpdateAndGetPadding(true)
	require.NoError(t, err)
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
	marshalledVP8, err = expectedVP8.Marshal()
	require.NoError(t, err)
	require.Equal(t, marshalledVP8, buf)
}

func TestVP8PictureIdWrapHandler(t *testing.T) {
	v := &VP8PictureIdWrapHandler{}

	v.Init(109, false)
	require.Equal(t, int32(109), v.MaxPictureId())
	require.False(t, v.maxMBit)

	v.UpdateMaxPictureId(109350, true)
	require.Equal(t, int32(109350), v.MaxPictureId())
	require.True(t, v.maxMBit)

	// start with something close to the 15-bit wrap around point
	v.Init(32766, true)

	// out-of-order, do not wrap
	extPictureId := v.Unwrap(32750, true)
	require.Equal(t, int32(32750), extPictureId)
	require.Equal(t, int32(0), v.totalWrap)
	require.Equal(t, int32(0), v.lastWrap)

	// wrap at 15-bits
	extPictureId = v.Unwrap(5, false)
	require.Equal(t, int32(32773), extPictureId) // 15-bit wrap at 32768 + 5 = 32773
	require.Equal(t, int32(32768), v.totalWrap)
	require.Equal(t, int32(32768), v.lastWrap)

	// set things near 7-bit wrap point
	v.UpdateMaxPictureId(32893, false) // 32768 + 125

	// wrap at 7-bits
	extPictureId = v.Unwrap(5, true)
	require.Equal(t, int32(32901), extPictureId) // 15-bit wrap at 32768 + 7-bit wrap at 128 + 5 =  32901
	require.Equal(t, int32(32896), v.totalWrap)  // one 15-bit wrap + one 7-bit wrap
	require.Equal(t, int32(128), v.lastWrap)

	// a new picture in 7-bit mode much with a gap in between.
	// A big enough gap which would have been treated as out-of-order in 7-bit mode.
	v.UpdateMaxPictureId(32901, false)
	extPictureId = v.Unwrap(73, false)
	require.Equal(t, int32(32841), extPictureId) // 15-bit wrap at 32768 + 73 =  32841

	// a new picture in 15-bit mode much with a gap in between.
	// A big enough gap which would have been treated as out-of-order in 7-bit mode.
	v.UpdateMaxPictureId(32901, true)
	v.lastWrap = int32(32768)
	extPictureId = v.Unwrap(73, false)
	require.Equal(t, int32(32969), extPictureId) // 15-bit wrap at 32768 + 7-bit wrap at 128 + 73 =  32969
}
