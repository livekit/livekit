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

package flexfec

import (
	"encoding/binary"
	"math/rand"
	"testing"

	pionflexfec "github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
)

const (
	testFECSSRC   = uint32(1234)
	testMediaSSRC = uint32(5678)
	testFECPT     = uint8(115)
	testMediaPT   = uint8(96)
)

func makeMediaPackets(t *testing.T, baseSN uint16, count int) []rtp.Packet {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(baseSN)))
	packets := make([]rtp.Packet, 0, count)
	for i := 0; i < count; i++ {
		payload := make([]byte, 100+rng.Intn(900))
		rng.Read(payload)
		packets = append(packets, rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    testMediaPT,
				SequenceNumber: baseSN + uint16(i),
				Timestamp:      3000 * uint32(i),
				SSRC:           testMediaSSRC,
				Marker:         i == count-1,
			},
			Payload: payload,
		})
	}
	return packets
}

func encodeFEC(t *testing.T, mediaPackets []rtp.Packet, numFEC uint32) []rtp.Packet {
	t.Helper()
	encoder := pionflexfec.NewFlexEncoder03(testFECPT, testFECSSRC)
	fecPackets := encoder.EncodeFec(mediaPackets, numFEC)
	require.NotEmpty(t, fecPackets)
	return fecPackets
}

func requirePacketEqual(t *testing.T, expected *rtp.Packet, actual *rtp.Packet) {
	t.Helper()
	require.Equal(t, expected.SequenceNumber, actual.SequenceNumber)
	require.Equal(t, expected.Timestamp, actual.Timestamp)
	require.Equal(t, expected.PayloadType, actual.PayloadType)
	require.Equal(t, expected.SSRC, actual.SSRC)
	require.Equal(t, expected.Marker, actual.Marker)
	require.Equal(t, expected.Payload, actual.Payload)
}

func TestDecoderRecoversSingleLoss(t *testing.T) {
	media := makeMediaPackets(t, 100, 5)
	fec := encodeFEC(t, media, 1)

	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())

	// drop media[2], feed the rest
	var recovered []*rtp.Packet
	for i := range media {
		if i == 2 {
			continue
		}
		recovered = append(recovered, decoder.DecodeFec(&media[i])...)
	}
	require.Empty(t, recovered)

	for i := range fec {
		recovered = append(recovered, decoder.DecodeFec(&fec[i])...)
	}

	require.Len(t, recovered, 1)
	requirePacketEqual(t, &media[2], recovered[0])

	stats := decoder.Stats()
	assert.Equal(t, uint64(len(fec)), stats.FECPacketsReceived)
	assert.Equal(t, uint64(1), stats.PacketsRecovered)
	assert.Equal(t, uint64(0), stats.FECPacketsDiscarded)
}

func TestDecoderRecoversWithLateMedia(t *testing.T) {
	// FEC arrives while two packets are missing; recovery happens once one of
	// them shows up late. Exercises updateCoveringFecPackets and the
	// stable-pointer window.
	media := makeMediaPackets(t, 200, 5)
	fec := encodeFEC(t, media, 1)

	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())

	var recovered []*rtp.Packet
	for _, i := range []int{0, 3, 4} {
		recovered = append(recovered, decoder.DecodeFec(&media[i])...)
	}
	for i := range fec {
		recovered = append(recovered, decoder.DecodeFec(&fec[i])...)
	}
	// two packets missing from the protected window, nothing recoverable yet
	require.Empty(t, recovered)

	// late arrival of media[1] leaves only media[2] missing
	recovered = decoder.DecodeFec(&media[1])
	require.Len(t, recovered, 1)
	requirePacketEqual(t, &media[2], recovered[0])
}

func TestDecoderRecoversMultipleWindows(t *testing.T) {
	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())
	encoder := pionflexfec.NewFlexEncoder03(testFECPT, testFECSSRC)

	var allRecovered []*rtp.Packet
	dropped := make(map[uint16]*rtp.Packet)
	baseSN := uint16(1000)
	for window := 0; window < 10; window++ {
		media := makeMediaPackets(t, baseSN, 10)
		fecPackets := encoder.EncodeFec(media, 1)
		require.NotEmpty(t, fecPackets)

		dropIdx := window % 10
		for i := range media {
			if i == dropIdx {
				dropped[media[i].SequenceNumber] = &media[i]
				continue
			}
			allRecovered = append(allRecovered, decoder.DecodeFec(&media[i])...)
		}
		for i := range fecPackets {
			allRecovered = append(allRecovered, decoder.DecodeFec(&fecPackets[i])...)
		}
		baseSN += 10
	}

	require.Len(t, allRecovered, 10)
	for _, rec := range allRecovered {
		expected, ok := dropped[rec.SequenceNumber]
		require.True(t, ok, "recovered unexpected sequence number %d", rec.SequenceNumber)
		requirePacketEqual(t, expected, rec)
	}
	assert.Equal(t, uint64(10), decoder.Stats().PacketsRecovered)
}

func TestDecoderSequenceNumberWrap(t *testing.T) {
	media := makeMediaPackets(t, 65533, 5) // spans 65533..1
	fec := encodeFEC(t, media, 1)

	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())

	var recovered []*rtp.Packet
	for i := range media {
		if i == 3 { // sequence number 0
			continue
		}
		recovered = append(recovered, decoder.DecodeFec(&media[i])...)
	}
	for i := range fec {
		recovered = append(recovered, decoder.DecodeFec(&fec[i])...)
	}

	require.Len(t, recovered, 1)
	requirePacketEqual(t, &media[3], recovered[0])
}

func TestDecoderDiscardsForeignProtectedSSRC(t *testing.T) {
	media := makeMediaPackets(t, 300, 5)
	fec := encodeFEC(t, media, 1)

	// decoder bound to a different protected stream
	decoder := NewDecoder(testFECSSRC, testMediaSSRC+1, logger.GetLogger())
	recovered := decoder.DecodeFec(&fec[0])
	require.Empty(t, recovered)

	stats := decoder.Stats()
	assert.Equal(t, uint64(1), stats.FECPacketsReceived)
	assert.Equal(t, uint64(1), stats.FECPacketsDiscarded)
}

func TestDecoderDiscardsMalformedFEC(t *testing.T) {
	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())

	for _, payload := range [][]byte{
		nil,
		{0x00},
		make([]byte, 10),
		func() []byte { // R bit set
			p := make([]byte, pionflexfec.BaseFec03HeaderSize+4)
			p[0] = 0x80
			p[8] = 1
			return p
		}(),
		func() []byte { // multiple protected ssrcs
			p := make([]byte, pionflexfec.BaseFec03HeaderSize+4)
			p[8] = 2
			return p
		}(),
		func() []byte { // empty mask
			p := make([]byte, pionflexfec.BaseFec03HeaderSize+4)
			p[8] = 1
			binary.BigEndian.PutUint32(p[12:], testMediaSSRC)
			binary.BigEndian.PutUint16(p[18:], 0x8000)
			return p
		}(),
	} {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    testFECPT,
				SequenceNumber: uint16(rand.Intn(65536)),
				SSRC:           testFECSSRC,
			},
			Payload: payload,
		}
		require.NotPanics(t, func() {
			require.Empty(t, decoder.DecodeFec(pkt))
		})
	}

	stats := decoder.Stats()
	assert.Equal(t, uint64(6), stats.FECPacketsReceived)
	assert.Equal(t, uint64(6), stats.FECPacketsDiscarded)
}

func TestDecoderDiscardsDuplicateFEC(t *testing.T) {
	media := makeMediaPackets(t, 400, 5)
	fec := encodeFEC(t, media, 1)

	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())
	for i := range media {
		decoder.DecodeFec(&media[i])
	}
	require.Empty(t, decoder.DecodeFec(&fec[0]))
	require.Empty(t, decoder.DecodeFec(&fec[0]))

	stats := decoder.Stats()
	assert.Equal(t, uint64(2), stats.FECPacketsReceived)
	assert.Equal(t, uint64(1), stats.FECPacketsDiscarded)
}

func TestDecoderInputMemoryReuse(t *testing.T) {
	// the decoder must not retain references to caller-owned packet memory
	media := makeMediaPackets(t, 500, 5)
	fec := encodeFEC(t, media, 1)

	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())

	scratch := &rtp.Packet{}
	feed := func(src *rtp.Packet) []*rtp.Packet {
		buf, err := src.Marshal()
		require.NoError(t, err)
		require.NoError(t, scratch.Unmarshal(buf))
		out := decoder.DecodeFec(scratch)
		// clobber the scratch memory the decoder saw
		for i := range scratch.Payload {
			scratch.Payload[i] = 0xde
		}
		return out
	}

	var recovered []*rtp.Packet
	for i := range media {
		if i == 2 {
			continue
		}
		recovered = append(recovered, feed(&media[i])...)
	}
	for i := range fec {
		recovered = append(recovered, feed(&fec[i])...)
	}

	require.Len(t, recovered, 1)
	requirePacketEqual(t, &media[2], recovered[0])
}

func TestDecoderTwoFECPacketsTwoLosses(t *testing.T) {
	// with 2 FEC packets over 10 media packets, the coverage interleaves, so
	// two losses landing in different coverage groups are both recoverable
	media := makeMediaPackets(t, 600, 10)
	fec := encodeFEC(t, media, 2)
	require.Len(t, fec, 2)

	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())

	var recovered []*rtp.Packet
	for i := range media {
		if i == 2 || i == 3 {
			continue
		}
		recovered = append(recovered, decoder.DecodeFec(&media[i])...)
	}
	for i := range fec {
		recovered = append(recovered, decoder.DecodeFec(&fec[i])...)
	}

	recoveredSNs := make(map[uint16]bool)
	for _, r := range recovered {
		recoveredSNs[r.SequenceNumber] = true
	}
	// at least one of the two losses must be recovered; both when the losses
	// fall in distinct coverage groups
	require.NotEmpty(t, recovered)
	for _, r := range recovered {
		expectedIdx := int(r.SequenceNumber - 600)
		requirePacketEqual(t, &media[expectedIdx], r)
	}
	require.True(t, recoveredSNs[602] || recoveredSNs[603])
}

func TestDecoderResetsOnBigSequenceGap(t *testing.T) {
	decoder := NewDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())

	media := makeMediaPackets(t, 100, 110)
	for i := range media {
		decoder.DecodeFec(&media[i])
	}

	// jump far ahead, decoder should reset its windows rather than misuse
	// stale state
	farMedia := makeMediaPackets(t, 30000, 5)
	fec := encodeFEC(t, farMedia, 1)
	var recovered []*rtp.Packet
	for i := range farMedia {
		if i == 1 {
			continue
		}
		recovered = append(recovered, decoder.DecodeFec(&farMedia[i])...)
	}
	for i := range fec {
		recovered = append(recovered, decoder.DecodeFec(&fec[i])...)
	}
	require.Len(t, recovered, 1)
	requirePacketEqual(t, &farMedia[1], recovered[0])
}
