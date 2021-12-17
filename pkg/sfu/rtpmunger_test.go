package sfu

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/testutils"
)

func TestSetLastSnTs(t *testing.T) {
	r := NewRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, err := testutils.GetTestExtPacket(params)
	require.NoError(t, err)
	require.NotNil(t, extPkt)

	r.SetLastSnTs(extPkt)
	require.True(t, r.highestIncomingSN == 23332)
	require.True(t, r.lastSN == 23333)
	require.True(t, r.lastTS == 0xabcdef)
	require.Equal(t, uint16(0), r.snOffset)
	require.Equal(t, uint32(0), r.tsOffset)

	params = &testutils.TestExtPacketParams{
		SequenceNumber: 0,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, err = testutils.GetTestExtPacket(params)
	require.NoError(t, err)
	require.NotNil(t, extPkt)

	r.SetLastSnTs(extPkt)
	require.True(t, r.highestIncomingSN == 65535)
	require.True(t, r.lastSN == 0)
	require.True(t, r.lastTS == 0xabcdef)
	require.Equal(t, uint16(0), r.snOffset)
	require.Equal(t, uint32(0), r.tsOffset)
}

func TestUpdateSnTsOffsets(t *testing.T) {
	r := NewRTPMunger()

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
	require.True(t, r.highestIncomingSN == 33332)
	require.True(t, r.lastSN == 23333)
	require.True(t, r.lastTS == 0xabcdef)
	require.EqualValues(t, 9999, r.snOffset)
	require.EqualValues(t, 0xffffffff, r.tsOffset)
}

func TestPacketDropped(t *testing.T) {
	r := NewRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)
	require.True(t, r.highestIncomingSN == 23332)
	require.True(t, r.lastSN == 23333)
	require.True(t, r.lastTS == 0xabcdef)
	require.EqualValues(t, 0, r.snOffset)
	require.EqualValues(t, 0, r.tsOffset)

	// drop a non-head packet, should cause no change in internals
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 33333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)
	r.PacketDropped(extPkt)
	require.True(t, r.highestIncomingSN == 23332)
	require.True(t, r.lastSN == 23333)
	require.EqualValues(t, 0, r.snOffset)

	// drop a head packet and check offset increases
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 44444,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)
	r.PacketDropped(extPkt)
	require.True(t, r.highestIncomingSN == 44444)
	require.True(t, r.lastSN == 23333)
	require.EqualValues(t, 1, r.snOffset)
}

func TestOutOfOrderSequenceNumber(t *testing.T) {
	r := NewRTPMunger()

	params := &testutils.TestExtPacketParams{
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)

	// out-of-order sequence number not in the missing sequence number cache
	params = &testutils.TestExtPacketParams{
		SequenceNumber: 23332,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
	}
	extPkt, _ = testutils.GetTestExtPacket(params)

	tpExpected := TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingOutOfOrder,
	}

	tp, err := r.UpdateAndGetSnTs(extPkt)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOutOfOrderSequenceNumberCacheMiss)
	require.True(t, reflect.DeepEqual(*tp, tpExpected))

	// add missing sequence number to the cache and try again
	r.missingSNs[23332] = 10

	tpExpected = TranslationParamsRTP{
		snOrdering:     SequenceNumberOrderingOutOfOrder,
		sequenceNumber: 23322,
		timestamp:      0xabcdef,
	}

	tp, err = r.UpdateAndGetSnTs(extPkt)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(*tp, tpExpected))
}

func TestDuplicateSequenceNumber(t *testing.T) {
	r := NewRTPMunger()

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
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
	require.True(t, reflect.DeepEqual(*tp, tpExpected))
}

func TestPaddingOnlyPacket(t *testing.T) {
	r := NewRTPMunger()

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
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
	require.True(t, reflect.DeepEqual(*tp, tpExpected))
	require.True(t, r.highestIncomingSN == 23333)
	require.True(t, r.lastSN == 23333)
	require.EqualValues(t, 1, r.snOffset)

	// padding only packet with a gap should not report an error
	params = &testutils.TestExtPacketParams{
		IsHead:         true,
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
	require.True(t, reflect.DeepEqual(*tp, tpExpected))
	require.True(t, r.highestIncomingSN == 23335)
	require.True(t, r.lastSN == 23334)
	require.EqualValues(t, 1, r.snOffset)
}

func TestGapInSequenceNumber(t *testing.T) {
	r := NewRTPMunger()

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
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
		IsHead:         true,
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
	require.True(t, reflect.DeepEqual(*tp, tpExpected))
	require.True(t, r.highestIncomingSN == 1)
	require.True(t, r.lastSN == 1)
	require.EqualValues(t, 0, r.snOffset)

	// ensure missing sequence numbers got recorded in cache

	// last received should not be in cache
	_, ok := r.missingSNs[65533]
	require.False(t, ok)

	// three after last received misisng with wrap-around
	offset, ok := r.missingSNs[65534]
	require.True(t, ok)
	require.Equal(t, uint16(0), offset)

	offset, ok = r.missingSNs[65535]
	require.True(t, ok)
	require.Equal(t, uint16(0), offset)

	offset, ok = r.missingSNs[0]
	require.True(t, ok)
	require.Equal(t, uint16(0), offset)

	// current received should not be in cache
	_, ok = r.missingSNs[1]
	require.False(t, ok)
}

func TestUpdateAndGetPaddingSnTs(t *testing.T) {
	r := NewRTPMunger()

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
		SequenceNumber: 23333,
		Timestamp:      0xabcdef,
		SSRC:           0x12345678,
		PayloadSize:    20,
	}
	extPkt, _ := testutils.GetTestExtPacket(params)
	r.SetLastSnTs(extPkt)

	// getting padding without forcing marker should fail
	_, err := r.UpdateAndGetPaddingSnTs(10, 10, 5, false)
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
			sequenceNumber: 23333 + uint16(i) + 1,
			timestamp:      0xabcdef + (uint32(i)*clockRate)/frameRate,
		}
	}
	snts, err := r.UpdateAndGetPaddingSnTs(numPadding, clockRate, frameRate, true)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(snts, sntsExpected))

	// now that there is a marker, timestamp should jump on first padding when asked again
	for i := 0; i < numPadding; i++ {
		sntsExpected[i] = SnTs{
			sequenceNumber: 23343 + uint16(i) + 1,
			timestamp:      0xabcdef + (uint32(i+1)*clockRate)/frameRate,
		}
	}
	snts, err = r.UpdateAndGetPaddingSnTs(numPadding, clockRate, frameRate, false)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(snts, sntsExpected))
}

func TestIsOnFrameBoundary(t *testing.T) {
	r := NewRTPMunger()

	params := &testutils.TestExtPacketParams{
		IsHead:         true,
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
		IsHead:         true,
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
