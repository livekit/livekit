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

package buffer

import (
	"math/rand"
	"testing"

	pionflexfec "github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"
	"github.com/pion/transport/v4/packetio"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fecTestMediaSSRC = uint32(0x11111111)
	fecTestFECSSRC   = uint32(0x22222222)
	fecTestFECPT     = uint8(115)
)

var flexfecCodec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeFlexFEC03,
		ClockRate:   90000,
		SDPFmtpLine: "repair-window=10000000",
	},
	PayloadType: webrtc.PayloadType(fecTestFECPT),
}

func fecTestMediaPackets(t *testing.T, baseSN uint16, count int) []rtp.Packet {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(baseSN)))
	pkts := make([]rtp.Packet, 0, count)
	for i := 0; i < count; i++ {
		payload := make([]byte, 50+rng.Intn(200))
		rng.Read(payload)
		// valid VP8 payload descriptor (S=1, no extensions) so the video
		// packet processing in the buffer accepts the packet
		payload[0] = 0x10
		sn := baseSN + uint16(i)
		pkts = append(pkts, rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    uint8(vp8Codec.PayloadType),
				SequenceNumber: sn,
				// derive timestamp from the sequence number so windows
				// generated separately stay monotonic
				Timestamp: 90000 + 3000*uint32(sn),
				SSRC:      fecTestMediaSSRC,
				Marker:    i == count-1,
			},
			Payload: payload,
		})
	}
	return pkts
}

func bindFECTestBuffer(t *testing.T, buff *Buffer) {
	t.Helper()
	buff.codecType = webrtc.RTPCodecTypeVideo
	require.NoError(t, buff.Bind(webrtc.RTPParameters{
		Codecs: []webrtc.RTPCodecParameters{vp8Codec, flexfecCodec},
	}, vp8Codec.RTPCodecCapability, 0))
}

func writePacket(t *testing.T, buff *Buffer, pkt *rtp.Packet) {
	t.Helper()
	raw, err := pkt.Marshal()
	require.NoError(t, err)
	_, err = buff.Write(raw)
	require.NoError(t, err)
}

// readExtSequenceNumbers drains count ExtPackets and returns sequence number
// -> extended sequence number of everything seen.
func readExtSequenceNumbers(t *testing.T, buff *Buffer, count int) map[uint16]uint64 {
	t.Helper()
	seen := make(map[uint16]uint64, count)
	var buf [1500]byte
	for i := 0; i < count; i++ {
		extPkt, err := buff.ReadExtended(buf[:])
		require.NoError(t, err)
		require.NotNil(t, extPkt)
		seen[extPkt.Packet.SequenceNumber] = extPkt.ExtSequenceNumber
	}
	return seen
}

// requireRecoveredInBucket asserts that the dropped packet was placed into
// the buffer's bucket (where downstream NACKs are served from), matching the
// behavior of RTX repaired packets.
func requireRecoveredInBucket(t *testing.T, buff *Buffer, dropped *rtp.Packet, extSNBySN map[uint16]uint64, refSN uint16) {
	t.Helper()
	refExtSN, ok := extSNBySN[refSN]
	require.True(t, ok, "reference sequence number %d not seen", refSN)
	droppedExtSN := refExtSN + uint64(dropped.SequenceNumber-refSN)

	var buf [1500]byte
	n, err := buff.GetPacket(buf[:], droppedExtSN)
	require.NoError(t, err, "recovered packet not found in bucket")

	var pkt rtp.Packet
	require.NoError(t, pkt.Unmarshal(buf[:n]))
	require.Equal(t, dropped.SequenceNumber, pkt.SequenceNumber)
	assert.Equal(t, dropped.Timestamp, pkt.Timestamp)
	assert.Equal(t, dropped.Payload, pkt.Payload)
}

func TestBufferFECRecoversDroppedPacket(t *testing.T) {
	factory := NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()

	primary := factory.GetOrNew(packetio.RTPBufferPacket, fecTestMediaSSRC).(*Buffer)
	fecBuff := factory.GetOrNew(packetio.RTPBufferPacket, fecTestFECSSRC).(*Buffer)
	factory.SetFECPair(fecTestFECSSRC, fecTestMediaSSRC)

	bindFECTestBuffer(t, primary)

	var recoveredDelta, receivedDelta int
	primary.OnFECRecovery(func(received int, recovered int, discarded int, bytesReceived int) {
		recoveredDelta += recovered
		receivedDelta += received
	})

	media := fecTestMediaPackets(t, 100, 10)
	encoder := pionflexfec.NewFlexEncoder03(fecTestFECPT, fecTestFECSSRC)
	fecPackets := encoder.EncodeFec(media, 2)
	require.NotEmpty(t, fecPackets)

	const droppedIdx = 3
	for i := range media {
		if i == droppedIdx {
			continue
		}
		writePacket(t, primary, &media[i])
	}
	for i := range fecPackets {
		writePacket(t, fecBuff, &fecPackets[i])
	}

	stats := primary.FECDecoderStats()
	assert.EqualValues(t, len(fecPackets), stats.FECPacketsReceived)
	assert.EqualValues(t, 1, stats.PacketsRecovered)
	assert.EqualValues(t, 0, stats.FECPacketsDiscarded)
	assert.Equal(t, 1, recoveredDelta)
	assert.Equal(t, len(fecPackets), receivedDelta)

	// the 9 received packets flow through the ext packet pipeline, the
	// recovered one fills the bucket like an RTX repair
	extSNBySN := readExtSequenceNumbers(t, primary, len(media)-1)
	requireRecoveredInBucket(t, primary, &media[droppedIdx], extSNBySN, media[0].SequenceNumber)
}

func TestBufferFECPairAfterPackets(t *testing.T) {
	// FEC packets arriving before the ssrc-group is known are queued as
	// pending and replayed when the pair is established. Media seen before
	// the pairing is not in the decoder window (cold start), so the first
	// window is not recoverable, subsequent windows are.
	factory := NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()

	primary := factory.GetOrNew(packetio.RTPBufferPacket, fecTestMediaSSRC).(*Buffer)
	bindFECTestBuffer(t, primary)

	encoder := pionflexfec.NewFlexEncoder03(fecTestFECPT, fecTestFECSSRC)
	media := fecTestMediaPackets(t, 200, 10)
	fecPackets := encoder.EncodeFec(media, 2)
	require.NotEmpty(t, fecPackets)

	for i := range media {
		if i == 5 {
			continue
		}
		writePacket(t, primary, &media[i])
	}

	// fec buffer created by first packet arrival, before the pair is declared
	fecBuff := factory.GetOrNew(packetio.RTPBufferPacket, fecTestFECSSRC).(*Buffer)
	for i := range fecPackets {
		writePacket(t, fecBuff, &fecPackets[i])
	}

	stats := primary.FECDecoderStats()
	require.EqualValues(t, 0, stats.FECPacketsReceived)

	factory.SetFECPair(fecTestFECSSRC, fecTestMediaSSRC)

	// pending FEC was replayed into the decoder, no recovery possible for the
	// cold-start window
	stats = primary.FECDecoderStats()
	assert.EqualValues(t, len(fecPackets), stats.FECPacketsReceived)
	assert.EqualValues(t, 0, stats.PacketsRecovered)

	// the next window recovers normally
	media2 := fecTestMediaPackets(t, 210, 10)
	fecPackets2 := encoder.EncodeFec(media2, 2)
	require.NotEmpty(t, fecPackets2)

	const droppedIdx = 4
	for i := range media2 {
		if i == droppedIdx {
			continue
		}
		writePacket(t, primary, &media2[i])
	}
	for i := range fecPackets2 {
		writePacket(t, fecBuff, &fecPackets2[i])
	}

	stats = primary.FECDecoderStats()
	assert.EqualValues(t, 1, stats.PacketsRecovered)

	extSNBySN := readExtSequenceNumbers(t, primary, len(media)-1+len(media2)-1)
	requireRecoveredInBucket(t, primary, &media2[droppedIdx], extSNBySN, media2[0].SequenceNumber)
}

func TestBufferFECCoupledBeforeBuffersExist(t *testing.T) {
	// pair declared first (from SDP), buffers created later on first packet
	factory := NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()
	factory.SetFECPair(fecTestFECSSRC, fecTestMediaSSRC)

	primary := factory.GetOrNew(packetio.RTPBufferPacket, fecTestMediaSSRC).(*Buffer)
	fecBuff := factory.GetOrNew(packetio.RTPBufferPacket, fecTestFECSSRC).(*Buffer)
	bindFECTestBuffer(t, primary)

	media := fecTestMediaPackets(t, 300, 5)
	encoder := pionflexfec.NewFlexEncoder03(fecTestFECPT, fecTestFECSSRC)
	fecPackets := encoder.EncodeFec(media, 1)
	require.NotEmpty(t, fecPackets)

	const droppedIdx = 2
	for i := range media {
		if i == droppedIdx {
			continue
		}
		writePacket(t, primary, &media[i])
	}
	for i := range fecPackets {
		writePacket(t, fecBuff, &fecPackets[i])
	}

	stats := primary.FECDecoderStats()
	assert.EqualValues(t, 1, stats.PacketsRecovered)

	extSNBySN := readExtSequenceNumbers(t, primary, len(media)-1)
	requireRecoveredInBucket(t, primary, &media[droppedIdx], extSNBySN, media[0].SequenceNumber)
}

func TestBufferFECIgnoresUnexpectedPayloadType(t *testing.T) {
	factory := NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()

	primary := factory.GetOrNew(packetio.RTPBufferPacket, fecTestMediaSSRC).(*Buffer)
	fecBuff := factory.GetOrNew(packetio.RTPBufferPacket, fecTestFECSSRC).(*Buffer)
	factory.SetFECPair(fecTestFECSSRC, fecTestMediaSSRC)

	// bound without flexfec in negotiated codecs
	primary.codecType = webrtc.RTPCodecTypeVideo
	require.NoError(t, primary.Bind(webrtc.RTPParameters{
		Codecs: []webrtc.RTPCodecParameters{vp8Codec},
	}, vp8Codec.RTPCodecCapability, 0))

	media := fecTestMediaPackets(t, 400, 5)
	encoder := pionflexfec.NewFlexEncoder03(fecTestFECPT, fecTestFECSSRC)
	fecPackets := encoder.EncodeFec(media, 1)
	require.NotEmpty(t, fecPackets)

	for i := range media {
		writePacket(t, primary, &media[i])
	}
	for i := range fecPackets {
		writePacket(t, fecBuff, &fecPackets[i])
	}

	// no flexfec payload type negotiated, decoder must not be created
	stats := primary.FECDecoderStats()
	assert.EqualValues(t, 0, stats.FECPacketsReceived)
	assert.EqualValues(t, 0, stats.PacketsRecovered)
}

func TestBufferFECNACKSuppression(t *testing.T) {
	// a recovered packet must clear the pending NACK for its sequence number
	factory := NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()

	primary := factory.GetOrNew(packetio.RTPBufferPacket, fecTestMediaSSRC).(*Buffer)
	fecBuff := factory.GetOrNew(packetio.RTPBufferPacket, fecTestFECSSRC).(*Buffer)
	factory.SetFECPair(fecTestFECSSRC, fecTestMediaSSRC)
	bindFECTestBuffer(t, primary)

	media := fecTestMediaPackets(t, 700, 10)
	encoder := pionflexfec.NewFlexEncoder03(fecTestFECPT, fecTestFECSSRC)
	fecPackets := encoder.EncodeFec(media, 2)
	require.NotEmpty(t, fecPackets)

	const droppedIdx = 6
	for i := range media {
		if i == droppedIdx {
			continue
		}
		writePacket(t, primary, &media[i])
	}

	// the only gap is the dropped packet, exactly one queued NACK
	require.NotNil(t, primary.nacker)
	require.Len(t, primary.nacker.Nacks(), 1, "expected queued NACK for dropped packet")

	for i := range fecPackets {
		writePacket(t, fecBuff, &fecPackets[i])
	}
	require.EqualValues(t, 1, primary.FECDecoderStats().PacketsRecovered)

	require.Empty(t, primary.nacker.Nacks(), "NACK for recovered packet not suppressed")
}
