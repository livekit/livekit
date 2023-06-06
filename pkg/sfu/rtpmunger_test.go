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
	require.Equal(t, uint16(23332), r.highestIncomingSN)
	require.Equal(t, uint16(23333), r.lastSN)
	require.Equal(t, uint32(0xabcdef), r.lastTS)
	require.Equal(t, uint16(0), r.snOffset)
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
	require.Equal(t, uint16(33332), r.highestIncomingSN)
	require.Equal(t, uint16(23333), r.lastSN)
	require.Equal(t, uint32(0xabcdef), r.lastTS)
	require.Equal(t, uint16(9999), r.snOffset)
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
	require.Equal(t, uint16(23332), r.highestIncomingSN)
	require.Equal(t, uint16(23333), r.lastSN)
	require.Equal(t, uint32(0xabcdef), r.lastTS)
	require.Equal(t, uint16(0), r.snOffset)
	require.Equal(t, uint32(0), r.tsOffset)

	r.UpdateAndGetSnTs(extPkt) // update sequence number offset
	require.Equal(t, 1, r.snOffsetsWritePtr)

	// drop a non-head packet, should cause no change in internals
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 33333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)
	r.PacketDropped(extPkt)
	require.Equal(t, uint16(23333), r.highestIncomingSN)
	require.Equal(t, uint16(23333), r.lastSN)
	require.Equal(t, uint16(0), r.snOffset)

	// drop a head packet and check offset increases
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 44444,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	r.UpdateAndGetSnTs(extPkt) // update sequence number offset
	snOffsetWritePtr := (44444 - 23333 + 1) & SnOffsetCacheMask
	require.Equal(t, snOffsetWritePtr, r.snOffsetsWritePtr)
	require.Equal(t, SnOffsetCacheSize, r.snOffsetsOccupancy)

	r.PacketDropped(extPkt)
	require.Equal(t, r.lastSN, uint16(44443))
	require.Equal(t, uint16(1), r.snOffset)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 44445,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	r.UpdateAndGetSnTs(extPkt) // update sequence number offset
	require.Equal(t, uint16(1), r.snOffsets[snOffsetWritePtr])
	snOffsetWritePtr = (snOffsetWritePtr + 1) & SnOffsetCacheMask
	require.Equal(t, snOffsetWritePtr, r.snOffsetsWritePtr)
	require.Equal(t, uint16(44444), r.lastSN)
	require.Equal(t, uint16(1), r.snOffset)
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

	// out-of-order sequence number not in the missing sequence number cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
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

	// add missing sequence number to the cache and try again
	r.snOffsets[SnOffsetCacheSize-1] = 10
	r.snOffsetsOccupancy++

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
	require.Equal(t, uint16(23333), r.highestIncomingSN)
	require.Equal(t, uint16(23333), r.lastSN)
	require.Equal(t, uint16(1), r.snOffset)

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
	require.Equal(t, uint16(23335), r.highestIncomingSN)
	require.Equal(t, uint16(23334), r.lastSN)
	require.Equal(t, uint16(1), r.snOffset)
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
	require.Equal(t, uint16(1), r.highestIncomingSN)
	require.Equal(t, uint16(1), r.lastSN)
	require.Equal(t, uint16(0), r.snOffset)

	// ensure missing sequence numbers got recorded in cache

	// last received, three missing in between and current received should all be in cache
	for i := uint16(65533); i != 2; i++ {
		offset, ok := r.getSnOffset(i)
		require.True(t, ok)
		require.Equal(t, uint16(0), offset)
	}

	// a padding only packet should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 2,
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
	require.Equal(t, uint16(2), r.highestIncomingSN)
	require.Equal(t, uint16(1), r.lastSN)
	require.Equal(t, uint16(1), r.snOffset)

	// a packet with a gap should be adding to missing cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 4,
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
	require.Equal(t, uint16(4), r.highestIncomingSN)
	require.Equal(t, uint16(3), r.lastSN)
	require.Equal(t, uint16(1), r.snOffset)

	// another contiguous padding only packet should be dropped
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 5,
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
	require.Equal(t, uint16(5), r.highestIncomingSN)
	require.Equal(t, uint16(3), r.lastSN)
	require.Equal(t, uint16(2), r.snOffset)

	// a packet with a gap should be adding to missing cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 7,
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
	require.Equal(t, uint16(7), r.highestIncomingSN)
	require.Equal(t, uint16(5), r.lastSN)
	require.Equal(t, uint16(2), r.snOffset)

	// check the missing packets
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 6,
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
	require.Equal(t, uint16(7), r.highestIncomingSN)
	require.Equal(t, uint16(5), r.lastSN)
	require.Equal(t, uint16(2), r.snOffset)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 3,
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
	require.Equal(t, uint16(7), r.highestIncomingSN)
	require.Equal(t, uint16(5), r.lastSN)
	require.Equal(t, uint16(2), r.snOffset)
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
