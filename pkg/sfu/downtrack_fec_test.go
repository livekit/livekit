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

package sfu

import (
	"math/rand"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/flexfec"
	"github.com/livekit/protocol/logger"
)

const (
	fecWriterTestMediaSSRC = uint32(0xaaaa1111)
	fecWriterTestFECSSRC   = uint32(0xbbbb2222)
	fecWriterTestFECPT     = uint8(115)
)

func fecWriterTestPacket(rng *rand.Rand, sn uint16) (*rtp.Header, []byte) {
	payload := make([]byte, 100+rng.Intn(400))
	rng.Read(payload)
	return &rtp.Header{
		Version:        2,
		PayloadType:    96,
		SequenceNumber: sn,
		Timestamp:      3000 * uint32(sn),
		SSRC:           fecWriterTestMediaSSRC,
		Marker:         sn%5 == 4,
	}, payload
}

func TestFECWriterEmitsOnWindowCompletion(t *testing.T) {
	w := newFECWriter(fecWriterTestFECSSRC, fecWriterTestFECPT, 5, 2, logger.GetLogger())
	rng := rand.New(rand.NewSource(1))

	var emitted []rtp.Packet
	for sn := uint16(100); sn < 110; sn++ {
		hdr, payload := fecWriterTestPacket(rng, sn)
		emitted = append(emitted, w.add(hdr, payload)...)
	}

	// two complete windows of 5, each yielding 2 FEC packets
	require.Len(t, emitted, 4)
	for _, fec := range emitted {
		assert.Equal(t, fecWriterTestFECSSRC, fec.SSRC)
		assert.Equal(t, fecWriterTestFECPT, fec.PayloadType)
	}

	stats := w.Stats()
	assert.EqualValues(t, 4, stats.PacketsSent)
	assert.NotZero(t, stats.BytesSent)
	assert.EqualValues(t, 0, stats.PartialWindows)
}

func TestFECWriterResetsOnDiscontinuity(t *testing.T) {
	w := newFECWriter(fecWriterTestFECSSRC, fecWriterTestFECPT, 5, 1, logger.GetLogger())
	rng := rand.New(rand.NewSource(2))

	// 3 packets, then a gap (e.g. an unrecovered upstream loss), then a
	// full window. The partial run before the gap is protected too.
	var emitted []rtp.Packet
	for _, sn := range []uint16{100, 101, 102, 110, 111, 112, 113, 114} {
		hdr, payload := fecWriterTestPacket(rng, sn)
		emitted = append(emitted, w.add(hdr, payload)...)
	}

	// one partial window (100-102) + one full window (110-114)
	require.Len(t, emitted, 2)
	stats := w.Stats()
	assert.EqualValues(t, 1, stats.PartialWindows)
	assert.EqualValues(t, 2, stats.PacketsSent)
}

func TestFECWriterDropsSinglePacketWindows(t *testing.T) {
	w := newFECWriter(fecWriterTestFECSSRC, fecWriterTestFECPT, 5, 1, logger.GetLogger())
	rng := rand.New(rand.NewSource(7))

	var emitted []rtp.Packet
	for _, sn := range []uint16{100, 110, 120, 121} {
		hdr, payload := fecWriterTestPacket(rng, sn)
		emitted = append(emitted, w.add(hdr, payload)...)
	}
	require.Empty(t, emitted)
	stats := w.Stats()
	assert.EqualValues(t, 2, stats.DiscardedSingles)
	assert.EqualValues(t, 0, stats.PacketsSent)
}

func TestFECWriterPartialWindowRecovers(t *testing.T) {
	// a partial window flushed on a gap must still produce usable FEC
	w := newFECWriter(fecWriterTestFECSSRC, fecWriterTestFECPT, 10, 2, logger.GetLogger())
	rng := rand.New(rand.NewSource(8))

	originals := make([]rtp.Packet, 0, 4)
	var emitted []rtp.Packet
	for _, sn := range []uint16{300, 301, 302, 303} {
		hdr, payload := fecWriterTestPacket(rng, sn)
		originals = append(originals, rtp.Packet{Header: hdr.Clone(), Payload: append([]byte(nil), payload...)})
		emitted = append(emitted, w.add(hdr, payload)...)
	}
	// gap triggers the partial flush
	hdr, payload := fecWriterTestPacket(rng, 310)
	emitted = append(emitted, w.add(hdr, payload)...)
	require.NotEmpty(t, emitted)

	decoder := flexfec.NewDecoder(fecWriterTestFECSSRC, fecWriterTestMediaSSRC, logger.GetLogger())
	for i := range originals {
		if i == 2 {
			continue
		}
		require.Empty(t, decoder.DecodeFec(&originals[i]))
	}
	recovered := decoder.DecodeFec(&emitted[0])
	require.Len(t, recovered, 1)
	assert.Equal(t, originals[2].SequenceNumber, recovered[0].SequenceNumber)
	assert.Equal(t, originals[2].Payload, recovered[0].Payload)
}

func TestFECWriterCopiesCallerMemory(t *testing.T) {
	w := newFECWriter(fecWriterTestFECSSRC, fecWriterTestFECPT, 3, 1, logger.GetLogger())
	rng := rand.New(rand.NewSource(3))

	originals := make([]rtp.Packet, 0, 3)
	scratchPayload := make([]byte, 1500)
	var emitted []rtp.Packet
	for sn := uint16(50); sn < 53; sn++ {
		hdr, payload := fecWriterTestPacket(rng, sn)
		originals = append(originals, rtp.Packet{Header: *hdr, Payload: append([]byte(nil), payload...)})

		// hand the writer reused scratch memory
		copy(scratchPayload, payload)
		emitted = append(emitted, w.add(hdr, scratchPayload[:len(payload)])...)
		for i := range scratchPayload {
			scratchPayload[i] = 0xee
		}
	}
	require.Len(t, emitted, 1)

	// the FEC packet must recover the original bytes, proving the writer
	// copied rather than aliased the scratch memory
	decoder := flexfec.NewDecoder(fecWriterTestFECSSRC, fecWriterTestMediaSSRC, logger.GetLogger())
	for i := range originals {
		if i == 1 {
			continue
		}
		require.Empty(t, decoder.DecodeFec(&originals[i]))
	}
	recovered := decoder.DecodeFec(&emitted[0])
	require.Len(t, recovered, 1)
	assert.Equal(t, originals[1].SequenceNumber, recovered[0].SequenceNumber)
	assert.Equal(t, originals[1].Payload, recovered[0].Payload)
	assert.Equal(t, originals[1].Timestamp, recovered[0].Timestamp)
}

func TestFECWriterRoundTripWithExtensions(t *testing.T) {
	// simulate the wire path: headers carry abs-send-time/transport-cc
	// extensions like the pacer emits them
	w := newFECWriter(fecWriterTestFECSSRC, fecWriterTestFECPT, 5, 1, logger.GetLogger())
	rng := rand.New(rand.NewSource(4))

	wirePackets := make([]rtp.Packet, 0, 5)
	var emitted []rtp.Packet
	for sn := uint16(800); sn < 805; sn++ {
		hdr, payload := fecWriterTestPacket(rng, sn)
		require.NoError(t, hdr.SetExtension(2, []byte{0x01, 0x02, 0x03}))
		twcc := []byte{byte(sn >> 8), byte(sn)}
		require.NoError(t, hdr.SetExtension(3, twcc))

		wirePackets = append(wirePackets, rtp.Packet{Header: hdr.Clone(), Payload: append([]byte(nil), payload...)})
		emitted = append(emitted, w.add(hdr, payload)...)
	}
	require.Len(t, emitted, 1)

	const droppedIdx = 2
	decoder := flexfec.NewDecoder(fecWriterTestFECSSRC, fecWriterTestMediaSSRC, logger.GetLogger())
	for i := range wirePackets {
		if i == droppedIdx {
			continue
		}
		require.Empty(t, decoder.DecodeFec(&wirePackets[i]))
	}
	recovered := decoder.DecodeFec(&emitted[0])
	require.Len(t, recovered, 1)

	expected, err := wirePackets[droppedIdx].Marshal()
	require.NoError(t, err)
	actual, err := recovered[0].Marshal()
	require.NoError(t, err)
	assert.Equal(t, expected, actual, "recovered wire bytes must match the original, extensions included")
}
