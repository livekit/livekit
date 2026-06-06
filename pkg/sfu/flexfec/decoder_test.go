// Copyright 2024 LiveKit, Inc.
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
	"testing"

	"github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

const (
	testMediaSSRC   = uint32(0x11223344)
	testFECSSRC     = uint32(0x55667788)
	testFECPayloadT = uint8(49)
	testMediaPT     = uint8(96)
)

func makeMediaPackets(baseSeq uint16, count int) []rtp.Packet {
	pkts := make([]rtp.Packet, 0, count)
	for i := 0; i < count; i++ {
		// vary the payload length to exercise length recovery
		payloadLen := 20 + (i % 5)
		payload := make([]byte, payloadLen)
		for j := range payload {
			payload[j] = byte((i*7 + j*3) & 0xff)
		}
		pkts = append(pkts, rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    testMediaPT,
				SequenceNumber: baseSeq + uint16(i),
				Timestamp:      uint32(1000 + i*960),
				SSRC:           testMediaSSRC,
			},
			Payload: payload,
		})
	}
	return pkts
}

func requirePacketsEqual(t *testing.T, want, got rtp.Packet) {
	t.Helper()
	require.Equal(t, want.SequenceNumber, got.SequenceNumber, "sequence number")
	require.Equal(t, want.SSRC, got.SSRC, "ssrc")
	require.Equal(t, want.PayloadType, got.PayloadType, "payload type")
	require.Equal(t, want.Timestamp, got.Timestamp, "timestamp")
	require.Equal(t, want.Payload, got.Payload, "payload")
}

func TestDecoderRecoversSingleLossMediaThenFEC(t *testing.T) {
	media := makeMediaPackets(1000, 10)

	encoder := flexfec.NewFlexEncoder03(testFECPayloadT, testFECSSRC)
	fecPackets := encoder.EncodeFec(cloneAll(media), 1)
	require.NotEmpty(t, fecPackets, "encoder should produce a FEC packet")

	dropped := 4
	dec := NewDecoder(testFECSSRC, testMediaSSRC)

	// deliver all media packets except the dropped one
	for i, p := range media {
		if i == dropped {
			continue
		}
		require.Empty(t, dec.Decode(p), "no recovery expected while feeding media")
	}

	// deliver the FEC packet - now exactly one protected packet is missing
	var recovered []rtp.Packet
	for _, fp := range fecPackets {
		recovered = append(recovered, dec.Decode(fp)...)
	}

	require.Len(t, recovered, 1)
	requirePacketsEqual(t, media[dropped], recovered[0])
}

func TestDecoderRecoversWhenFECArrivesFirst(t *testing.T) {
	media := makeMediaPackets(2000, 8)

	encoder := flexfec.NewFlexEncoder03(testFECPayloadT, testFECSSRC)
	fecPackets := encoder.EncodeFec(cloneAll(media), 1)
	require.NotEmpty(t, fecPackets)

	dropped := 2
	dec := NewDecoder(testFECSSRC, testMediaSSRC)

	// FEC arrives before any media: nothing can be recovered yet
	for _, fp := range fecPackets {
		require.Empty(t, dec.Decode(fp))
	}

	var recovered []rtp.Packet
	for i, p := range media {
		if i == dropped {
			continue
		}
		recovered = append(recovered, dec.Decode(p)...)
	}

	require.Len(t, recovered, 1)
	requirePacketsEqual(t, media[dropped], recovered[0])
}

func TestDecoderNoLossNoRecovery(t *testing.T) {
	media := makeMediaPackets(3000, 6)

	encoder := flexfec.NewFlexEncoder03(testFECPayloadT, testFECSSRC)
	fecPackets := encoder.EncodeFec(cloneAll(media), 1)
	require.NotEmpty(t, fecPackets)

	dec := NewDecoder(testFECSSRC, testMediaSSRC)
	for _, p := range media {
		require.Empty(t, dec.Decode(p))
	}
	for _, fp := range fecPackets {
		require.Empty(t, dec.Decode(fp), "no recovery expected when nothing is lost")
	}
}

func TestDecoderCannotRecoverDoubleLoss(t *testing.T) {
	media := makeMediaPackets(4000, 10)

	encoder := flexfec.NewFlexEncoder03(testFECPayloadT, testFECSSRC)
	fecPackets := encoder.EncodeFec(cloneAll(media), 1)
	require.NotEmpty(t, fecPackets)

	dec := NewDecoder(testFECSSRC, testMediaSSRC)
	// drop two packets covered by a single FEC packet -> unrecoverable
	for i, p := range media {
		if i == 3 || i == 6 {
			continue
		}
		dec.Decode(p)
	}

	var recovered []rtp.Packet
	for _, fp := range fecPackets {
		recovered = append(recovered, dec.Decode(fp)...)
	}
	require.Empty(t, recovered, "single FEC packet cannot recover two losses")
}

func TestDecoderIgnoresUnrelatedSSRC(t *testing.T) {
	dec := NewDecoder(testFECSSRC, testMediaSSRC)
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, SSRC: 0xdeadbeef, SequenceNumber: 1},
		Payload: []byte{1, 2, 3},
	}
	require.Empty(t, dec.Decode(pkt))
}

func cloneAll(pkts []rtp.Packet) []rtp.Packet {
	out := make([]rtp.Packet, len(pkts))
	for i, p := range pkts {
		out[i] = clonePacket(p)
	}
	return out
}
