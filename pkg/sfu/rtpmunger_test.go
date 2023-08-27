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

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/testutils"
)

func newRTPMunger() *RTPMunger {
	return NewRTPMunger(logger.GetLogger())
}

func TestSetLastSnTs(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, err := testutils.GetTestExtPacket(params)
	require.NoError(t, err)
	require.NotNil(t, extPkt)

	r.SetLastSnTs(extPkt)
	require.Equal(t, uint32(23332), r.extHighestIncomingSN)
	require.Equal(t, uint32(23333), r.extLastSN)
	require.Equal(t, uint32(0xabcdef), r.lastTS)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(0), snOffset)
	require.Equal(t, uint32(0), r.tsOffset)
}

func TestUpdateSnTsOffsets(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 33333,
		Timestamp:      0xabcdef,
		SSRC:           0x87654321,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)
	r.UpdateSnTsOffsets(extPkt, 1, 1)
	require.Equal(t, uint32(33332), r.extHighestIncomingSN)
	require.Equal(t, uint32(23333), r.extLastSN)
	require.Equal(t, uint32(0xabcdef), r.lastTS)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(9999), snOffset)
	require.Equal(t, uint32(0xffffffff), r.tsOffset)
}

func TestPacketDropped(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    10,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)
	require.Equal(t, uint32(23332), r.extHighestIncomingSN)
	require.Equal(t, uint32(23333), r.extLastSN)
	require.Equal(t, uint32(0xabcdef), r.lastTS)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(0), snOffset)
	require.Equal(t, uint32(0), r.tsOffset)

	r.UpdateAndGetSnTs(extPkt) // update sequence number offset

	// drop a non-head packet, should cause no change in internals
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 33333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)
	r.PacketDropped(extPkt)
	require.Equal(t, uint32(23333), r.extHighestIncomingSN)
	require.Equal(t, uint32(23333), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(0), snOffset)

	// drop a head packet and check offset increases
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 44444,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	r.UpdateAndGetSnTs(extPkt) // update sequence number offset

	r.PacketDropped(extPkt)
	require.Equal(t, uint32(44443), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(1), snOffset)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 44445,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	r.UpdateAndGetSnTs(extPkt) // update sequence number offset
	require.Equal(t, r.extLastSN, uint32(44444))
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(1), snOffset)
}

func TestOutOfOrderSequenceNumber(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    10,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)
	r.UpdateAndGetSnTs(extPkt)

	// add a missing sequence number to the cache
	r.snRangeMap.IncValue(10)
	r.snRangeMap.AddRange(23332, 23333)

	// out-of-order sequence number not in the missing sequence number cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23331,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    10,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected := TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingOutOfOrder,
	}

	tp, err := r.UpdateAndGetSnTs(extPkt)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOutOfOrderSequenceNumberCacheMiss)
	require.Equal(t, tpExpected, *tp)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    10,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:     SequenceNumberOrderingOutOfOrder,
		sequenceNumber: 23322,
		timestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.Equal(t, tpExpected, *tp)
}

func TestDuplicateSequenceNumber(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)

	// send first packet through
	r.UpdateAndGetSnTs(extPkt)

	// send it again - duplicate packet
	tpExpected := TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingDuplicate,
	}

	tp, err := r.UpdateAndGetSnTs(extPkt)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDuplicatePacket)
	require.Equal(t, tpExpected, *tp)
}

func TestPaddingOnlyPacket(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)

	// contiguous padding only packet should report an error
	tpExpected := TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingContiguous,
	}

	tp, err := r.UpdateAndGetSnTs(extPkt)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPaddingOnlyPacket)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(23333), r.extHighestIncomingSN)
	require.Equal(t, uint32(23333), r.extLastSN)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(1), snOffset)

	// padding only packet with a gap should not report an error
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23335,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:     SequenceNumberOrderingGap,
		sequenceNumber: 23334,
		timestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(23335), r.extHighestIncomingSN)
	require.Equal(t, uint32(23334), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(1), snOffset)
}

func TestGapInSequenceNumber(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 65533,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    33,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)

	_, err := r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)

	// three lost packets
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 1,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    33,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected := TranslationParamsRTP{
		snOrdering:     SequenceNumberOrderingGap,
		sequenceNumber: 1,
		timestamp:      0xabcdef,
	}

	tp, err := r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(65536+1), r.extHighestIncomingSN)
	require.Equal(t, uint32(65536+1), r.extLastSN)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(0), snOffset)

	// ensure missing sequence numbers got recorded in cache

	// last received, three missing in between and current received should all be in cache
	for i := uint32(65534); i != 65536+1; i++ {
		offset, err := r.snRangeMap.GetValue(i)
		require.NoError(t, err)
		require.Equal(t, uint32(0), offset)
	}

	// a padding only packet should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 2,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingContiguous,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.ErrorIs(t, err, ErrPaddingOnlyPacket)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(65536+2), r.extHighestIncomingSN)
	require.Equal(t, uint32(65536+1), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(1), snOffset)

	// a packet with a gap should be adding to missing cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 4,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    22,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:     SequenceNumberOrderingGap,
		sequenceNumber: 3,
		timestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(65536+4), r.extHighestIncomingSN)
	require.Equal(t, uint32(65536+3), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(1), snOffset)

	// another contiguous padding only packet should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 5,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingContiguous,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.ErrorIs(t, err, ErrPaddingOnlyPacket)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(65536+5), r.extHighestIncomingSN)
	require.Equal(t, uint32(65536+3), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(2), snOffset)

	// a packet with a gap should be adding to missing cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 7,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    22,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:     SequenceNumberOrderingGap,
		sequenceNumber: 5,
		timestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(65536+7), r.extHighestIncomingSN)
	require.Equal(t, uint32(65536+5), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(2), snOffset)

	// check the missing packets
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 6,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:     SequenceNumberOrderingOutOfOrder,
		sequenceNumber: 4,
		timestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(65536+7), r.extHighestIncomingSN)
	require.Equal(t, uint32(65536+5), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(2), snOffset)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 3,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:     SequenceNumberOrderingOutOfOrder,
		sequenceNumber: 2,
		timestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.Equal(t, tpExpected, *tp)
	require.Equal(t, uint32(65536+7), r.extHighestIncomingSN)
	require.Equal(t, uint32(65536+5), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint32(2), snOffset)
}

func TestUpdateAndGetPaddingSnTs(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)

	// getting padding without forcing marker should fail
	_, err := r.UpdateAndGetPaddingSnTs(10, 10, 5, false, 0)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPaddingNotOnFrameBoundary)

	// forcing a marker should not error out.
	// And timestamp on first padding should be the same as the last one.
	numPadding := 10
	clockRate := uint32(10)
	frameRate := uint32(5)
	var sntsExpected = make([]SnTs, numPadding)
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			sequenceNumber: params.SequenceNumber + uint16(i) + 1,
			timestamp:      params.Timestamp + ((uint32(i)*clockRate)+frameRate-1)/frameRate,
		}
	}
	snts, err := r.UpdateAndGetPaddingSnTs(numPadding, clockRate, frameRate, true, params.Timestamp)
	require.NoError(t, err)
	require.Equal(t, sntsExpected, snts)

	// now that there is a marker, timestamp should jump on first padding when asked again
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			sequenceNumber: params.SequenceNumber + uint16(len(snts)) + uint16(i) + 1,
			timestamp:      snts[len(snts)-1].timestamp + ((uint32(i+1)*clockRate)+frameRate-1)/frameRate,
		}
	}
	snts, err = r.UpdateAndGetPaddingSnTs(numPadding, clockRate, frameRate, false, snts[len(snts)-1].timestamp)
	require.NoError(t, err)
	require.Equal(t, sntsExpected, snts)
}

func TestIsOnFrameBoundary(t *testing.T) {
	r := newRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)

	// send it through
	_, err := r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.False(t, r.IsOnFrameBoundary())

	// packet with RTP marker
	params = &testutils.TestExtPacketParams{
		SetMarker:      true,
		SequenceNumber: 23334,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	// send it through
	_, err = r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.True(t, r.IsOnFrameBoundary())
}
