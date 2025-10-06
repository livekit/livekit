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
	require.Equal(t, uint64(23332), r.extHighestIncomingSN)
	require.Equal(t, uint64(23333), r.extLastSN)
	require.Equal(t, uint64(0xabcdef), r.extLastTS)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.Error(t, err)
	snOffset, err = r.snRangeMap.GetValue(r.extLastSN)
	require.NoError(t, err)
	require.Equal(t, uint64(0), snOffset)
	require.Equal(t, uint64(0), r.snOffset)
	require.Equal(t, uint64(0), r.tsOffset)
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
	require.Equal(t, uint64(33332), r.extHighestIncomingSN)
	require.Equal(t, uint64(23333), r.extLastSN)
	require.Equal(t, uint64(0xabcdef), r.extLastTS)
	_, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.Error(t, err)
	_, err = r.snRangeMap.GetValue(r.extLastSN)
	require.Error(t, err)
	require.Equal(t, uint64(9999), r.snOffset)
	require.Equal(t, uint64(0xffff_ffff_ffff_ffff), r.tsOffset)
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
	require.Equal(t, uint64(23332), r.extHighestIncomingSN)
	require.Equal(t, uint64(23333), r.extLastSN)
	require.Equal(t, uint64(0xabcdef), r.extLastTS)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.Error(t, err)
	snOffset, err = r.snRangeMap.GetValue(r.extLastSN)
	require.NoError(t, err)
	require.Equal(t, uint64(0), snOffset)
	require.Equal(t, uint64(0), r.tsOffset)

	r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker) // update sequence number offset

	// drop a non-head packet, should cause no change in internals
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 33333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)
	r.PacketDropped(extPkt)
	require.Equal(t, uint64(23333), r.extHighestIncomingSN)
	require.Equal(t, uint64(23333), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint64(0), snOffset)

	// drop a head packet and check offset increases
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 44444,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker) // update sequence number offset
	require.Equal(t, uint64(44444), r.extLastSN)

	r.PacketDropped(extPkt)
	require.Equal(t, uint64(23333), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.Error(t, err)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN + 1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), snOffset)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 44445,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker) // update sequence number offset
	require.Equal(t, r.extLastSN, uint64(44444))
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint64(1), snOffset)
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
	r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)

	// should not be able to add a missing sequence number to the cache that is before start
	err := r.snRangeMap.ExcludeRange(23332, 23333)
	require.Error(t, err)

	// out-of-order sequence number before start should miss
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23331,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    10,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tp, err := r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.Error(t, err)

	//  add a missing sequence number to the cache
	err = r.snRangeMap.ExcludeRange(23334, 23335)
	require.NoError(t, err)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23336,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    10,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)
	r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)

	// out-of-order sequence number should be munged from cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23335,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    10,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected := TranslationParamsRTP{
		snOrdering:        SequenceNumberOrderingOutOfOrder,
		extSequenceNumber: 23334,
		extTimestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.Equal(t, tpExpected, tp)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    10,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingOutOfOrder,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.Error(t, err, ErrOutOfOrderSequenceNumberCacheMiss)
	require.Equal(t, tpExpected, tp)
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
	r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)

	// send it again - duplicate packet
	tpExpected := TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingDuplicate,
	}

	tp, err := r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.ErrorIs(t, err, ErrDuplicatePacket)
	require.Equal(t, tpExpected, tp)
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

	tp, err := r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPaddingOnlyPacket)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(23333), r.extHighestIncomingSN)
	require.Equal(t, uint64(23333), r.extLastSN)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.Error(t, err)

	// padding only packet with a gap should not report an error
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23335,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:        SequenceNumberOrderingGap,
		extSequenceNumber: 23334,
		extTimestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(23335), r.extHighestIncomingSN)
	require.Equal(t, uint64(23334), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint64(1), snOffset)
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

	_, err := r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
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
		snOrdering:        SequenceNumberOrderingGap,
		extSequenceNumber: 65536 + 1,
		extTimestamp:      0xabcdef,
	}

	tp, err := r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(65536+1), r.extHighestIncomingSN)
	require.Equal(t, uint64(65536+1), r.extLastSN)
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint64(0), snOffset)

	// ensure missing sequence numbers have correct cached offset
	for i := uint64(65534); i != 65536+1; i++ {
		offset, err := r.snRangeMap.GetValue(i)
		require.NoError(t, err)
		require.Equal(t, uint64(0), offset)
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

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.ErrorIs(t, err, ErrPaddingOnlyPacket)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(65536+2), r.extHighestIncomingSN)
	require.Equal(t, uint64(65536+1), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.Error(t, err)

	// a packet with a gap should be adjusting for dropped padding packet
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 4,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    22,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:        SequenceNumberOrderingGap,
		extSequenceNumber: 65536 + 3,
		extTimestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(65536+4), r.extHighestIncomingSN)
	require.Equal(t, uint64(65536+3), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint64(1), snOffset)

	// ensure missing sequence number has correct cached offset
	offset, err := r.snRangeMap.GetValue(65536 + 3)
	require.NoError(t, err)
	require.Equal(t, uint64(1), offset)

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

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.ErrorIs(t, err, ErrPaddingOnlyPacket)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(65536+5), r.extHighestIncomingSN)
	require.Equal(t, uint64(65536+3), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.Error(t, err)

	// a packet with a gap should be adjusting for dropped packets
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 7,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    22,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:        SequenceNumberOrderingGap,
		extSequenceNumber: 65536 + 5,
		extTimestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(65536+7), r.extHighestIncomingSN)
	require.Equal(t, uint64(65536+5), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint64(2), snOffset)

	// ensure missing sequence number has correct cached offset
	offset, err = r.snRangeMap.GetValue(65536 + 3)
	require.NoError(t, err)
	require.Equal(t, uint64(1), offset)

	offset, err = r.snRangeMap.GetValue(65536 + 6)
	require.NoError(t, err)
	require.Equal(t, uint64(2), offset)

	// check the missing packets
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 6,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:        SequenceNumberOrderingOutOfOrder,
		extSequenceNumber: 65536 + 4,
		extTimestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(65536+7), r.extHighestIncomingSN)
	require.Equal(t, uint64(65536+5), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint64(2), snOffset)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 3,
		SNCycles:       1,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected = TranslationParamsRTP{
		snOrdering:        SequenceNumberOrderingOutOfOrder,
		extSequenceNumber: 65536 + 2,
		extTimestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.Equal(t, tpExpected, tp)
	require.Equal(t, uint64(65536+7), r.extHighestIncomingSN)
	require.Equal(t, uint64(65536+5), r.extLastSN)
	snOffset, err = r.snRangeMap.GetValue(r.extHighestIncomingSN)
	require.NoError(t, err)
	require.Equal(t, uint64(2), snOffset)
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
	clockRate := uint64(10)
	frameRate := uint64(5)
	var sntsExpected = make([]SnTs, numPadding)
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			extSequenceNumber: uint64(params.SequenceNumber) + uint64(i) + 1,
			extTimestamp:      uint64(params.Timestamp) + ((uint64(i)*clockRate)+frameRate-1)/frameRate,
		}
	}
	snts, err := r.UpdateAndGetPaddingSnTs(numPadding, uint32(clockRate), uint32(frameRate), true, extPkt.ExtTimestamp)
	require.NoError(t, err)
	require.Equal(t, sntsExpected, snts)

	// now that there is a marker, timestamp should jump on first padding when asked again
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			extSequenceNumber: uint64(params.SequenceNumber) + uint64(len(snts)) + uint64(i) + 1,
			extTimestamp:      snts[len(snts)-1].extTimestamp + ((uint64(i+1)*clockRate)+frameRate-1)/frameRate,
		}
	}
	snts, err = r.UpdateAndGetPaddingSnTs(numPadding, uint32(clockRate), uint32(frameRate), false, snts[len(snts)-1].extTimestamp)
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
	_, err := r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.False(t, r.IsOnFrameBoundary())

	// packet with RTP marker
	params = &testutils.TestExtPacketParams{
		Marker:         true,
		SequenceNumber: 23334,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	// send it through
	_, err = r.UpdateAndGetSnTs(extPkt, extPkt.Packet.Marker)
	require.NoError(t, err)
	require.True(t, r.IsOnFrameBoundary())
}
