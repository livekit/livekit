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

package buffer

import (
	"testing"
	"time"

	"github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"
	"github.com/pion/transport/v4/packetio"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// TestPublisherToSFUFlexFECRecoveryThroughFactory exercises the publisher -> SFU receive
// path: the source and repair buffers are created through the factory (as the SRTP layer
// would), associated via SetFECPair, and a lost publisher packet is recovered from the
// FlexFEC repair stream and forwarded out of the source buffer.
func TestPublisherToSFUFlexFECRecoveryThroughFactory(t *testing.T) {
	const (
		mediaSSRC = uint32(0xAABBCCDD)
		fecSSRC   = uint32(0x11223344)
		baseSeq   = uint16(5000)
		numMedia  = 10
		dropIdx   = 6
	)

	mediaBuf, fecBuf := newFECBufferPair(t, mediaSSRC, fecSSRC)
	defer mediaBuf.Close()
	defer fecBuf.Close()

	media := makeFECTestMediaPackets(mediaSSRC, baseSeq, numMedia)
	fecPackets := makeFECTestRepairPackets(t, media, fecSSRC)
	got := collectExtPackets(mediaBuf)

	writeMediaExcept(t, mediaBuf, media, dropIdx)
	writeRepairPackets(t, fecBuf, fecPackets)

	requireRecoveredPacket(t, got, media[dropIdx])
}

// TestPublisherToSFUFlexFECRecoveryReplaysBufferedRepairPackets covers the ordering that
// can happen on ingress: a FlexFEC repair packet reaches the buffer before the SDP-derived
// FEC-FR association has been applied. The repair buffer must replay its queued packet
// into the source decoder once SetFECPair wires the streams together.
func TestPublisherToSFUFlexFECRecoveryReplaysBufferedRepairPackets(t *testing.T) {
	const (
		mediaSSRC = uint32(0x01020304)
		fecSSRC   = uint32(0x05060708)
		baseSeq   = uint16(9000)
		numMedia  = 10
		dropIdx   = 3
	)

	factory := NewFactoryOfBufferFactory(InitPacketBufferSizeVideo, InitPacketBufferSizeAudio).CreateBufferFactory()

	fecBuf, ok := factory.GetOrNew(packetio.RTPBufferPacket, fecSSRC).(*Buffer)
	require.True(t, ok)
	defer fecBuf.Close()

	media := makeFECTestMediaPackets(mediaSSRC, baseSeq, numMedia)
	fecPackets := makeFECTestRepairPackets(t, media, fecSSRC)

	// Repair arrives before the source buffer and FEC-FR association are known. Because
	// fecBuf is not associated yet, Buffer queues the packet in pPackets.
	writeRepairPackets(t, fecBuf, fecPackets)

	mediaBuf, ok := factory.GetOrNew(packetio.RTPBufferPacket, mediaSSRC).(*Buffer)
	require.True(t, ok)
	defer mediaBuf.Close()
	bindVideoBuffer(t, mediaBuf)

	got := collectExtPackets(mediaBuf)

	factory.SetFECPair(fecSSRC, mediaSSRC)
	require.NotNil(t, mediaBuf.fecDecoder)
	require.Equal(t, mediaBuf, fecBuf.primaryBufferForFEC)

	writeMediaExcept(t, mediaBuf, media, dropIdx)

	requireRecoveredPacket(t, got, media[dropIdx])
}

func TestPublisherToSFUFlexFECPairRememberedBeforeBuffers(t *testing.T) {
	for _, tc := range []struct {
		name              string
		createSourceFirst bool
	}{
		{name: "source_first", createSourceFirst: true},
		{name: "repair_first", createSourceFirst: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const (
				mediaSSRC = uint32(0x0A0B0C0D)
				fecSSRC   = uint32(0x01010101)
				baseSeq   = uint16(12000)
				numMedia  = 10
				dropIdx   = 5
			)

			factory := NewFactoryOfBufferFactory(InitPacketBufferSizeVideo, InitPacketBufferSizeAudio).CreateBufferFactory()
			factory.SetFECPair(fecSSRC, mediaSSRC)

			var mediaBuf *Buffer
			var fecBuf *Buffer
			if tc.createSourceFirst {
				mediaBuf = getRTPBuffer(t, factory, mediaSSRC)
				bindVideoBuffer(t, mediaBuf)
				fecBuf = getRTPBuffer(t, factory, fecSSRC)
			} else {
				fecBuf = getRTPBuffer(t, factory, fecSSRC)
				mediaBuf = getRTPBuffer(t, factory, mediaSSRC)
				bindVideoBuffer(t, mediaBuf)
			}
			defer mediaBuf.Close()
			defer fecBuf.Close()

			require.NotNil(t, mediaBuf.fecDecoder)
			require.Equal(t, mediaBuf, fecBuf.primaryBufferForFEC)

			media := makeFECTestMediaPackets(mediaSSRC, baseSeq, numMedia)
			fecPackets := makeFECTestRepairPackets(t, media, fecSSRC)
			got := collectExtPackets(mediaBuf)

			writeMediaExcept(t, mediaBuf, media, dropIdx)
			writeRepairPackets(t, fecBuf, fecPackets)

			requireRecoveredPacket(t, got, media[dropIdx])
		})
	}
}

func newFECBufferPair(t *testing.T, mediaSSRC, fecSSRC uint32) (*Buffer, *Buffer) {
	t.Helper()

	factory := NewFactoryOfBufferFactory(InitPacketBufferSizeVideo, InitPacketBufferSizeAudio).CreateBufferFactory()

	mediaBuf := getRTPBuffer(t, factory, mediaSSRC)
	bindVideoBuffer(t, mediaBuf)

	fecBuf := getRTPBuffer(t, factory, fecSSRC)

	factory.SetFECPair(fecSSRC, mediaSSRC)
	require.NotNil(t, mediaBuf.fecDecoder)
	require.Equal(t, mediaBuf, fecBuf.primaryBufferForFEC)

	return mediaBuf, fecBuf
}

func getRTPBuffer(t *testing.T, factory *Factory, ssrc uint32) *Buffer {
	t.Helper()

	b, ok := factory.GetOrNew(packetio.RTPBufferPacket, ssrc).(*Buffer)
	require.True(t, ok)
	return b
}

func bindVideoBuffer(t *testing.T, b *Buffer) {
	t.Helper()

	b.codecType = webrtc.RTPCodecTypeVideo
	require.NoError(t, b.Bind(
		webrtc.RTPParameters{Codecs: []webrtc.RTPCodecParameters{vp8Codec}},
		vp8Codec.RTPCodecCapability,
		0,
	))
}

func makeFECTestMediaPackets(mediaSSRC uint32, baseSeq uint16, numMedia int) []rtp.Packet {
	media := make([]rtp.Packet, numMedia)
	for i := range media {
		payload := make([]byte, 16)
		for j := range payload {
			payload[j] = byte((i*11 + j*5 + 1) & 0xff)
		}
		media[i] = rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    uint8(vp8Codec.PayloadType),
				SequenceNumber: baseSeq + uint16(i),
				Timestamp:      uint32(8000 + i*960),
				SSRC:           mediaSSRC,
			},
			Payload: payload,
		}
	}
	return media
}

func makeFECTestRepairPackets(t *testing.T, media []rtp.Packet, fecSSRC uint32) []rtp.Packet {
	t.Helper()

	encoder := flexfec.NewFlexEncoder03(uint8(49), fecSSRC)
	mediaForFec := make([]rtp.Packet, len(media))
	copy(mediaForFec, media)
	fecPackets := encoder.EncodeFec(mediaForFec, 1)
	require.NotEmpty(t, fecPackets)
	return fecPackets
}

func collectExtPackets(b *Buffer) <-chan *ExtPacket {
	got := make(chan *ExtPacket, 32)
	go func() {
		var buf [1500]byte
		for {
			ep, err := b.ReadExtended(buf[:])
			if err != nil {
				return
			}
			if ep != nil {
				clone := *ep
				got <- &clone
			}
		}
	}()
	return got
}

func writeMediaExcept(t *testing.T, b *Buffer, media []rtp.Packet, dropIdx int) {
	t.Helper()

	for i := range media {
		if i == dropIdx {
			continue
		}
		raw, err := media[i].Marshal()
		require.NoError(t, err)
		_, err = b.Write(raw)
		require.NoError(t, err)
	}
}

func writeRepairPackets(t *testing.T, b *Buffer, fecPackets []rtp.Packet) {
	t.Helper()

	for _, fp := range fecPackets {
		raw, err := fp.Marshal()
		require.NoError(t, err)
		_, err = b.Write(raw)
		require.NoError(t, err)
	}
}

func requireRecoveredPacket(t *testing.T, got <-chan *ExtPacket, want rtp.Packet) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	seen := map[uint16]*ExtPacket{}
	for {
		select {
		case ep := <-got:
			seen[ep.Packet.SequenceNumber] = ep
			if rec, found := seen[want.SequenceNumber]; found {
				require.Equal(t, want.SSRC, rec.Packet.SSRC)
				require.Equal(t, want.Payload, rec.Packet.Payload)
				require.Equal(t, want.Timestamp, rec.Packet.Timestamp)
				return
			}
		case <-deadline:
			t.Fatalf("recovered packet seq %d not forwarded; saw %v", want.SequenceNumber, seqKeys(seen))
		}
	}
}

func seqKeys(m map[uint16]*ExtPacket) []uint16 {
	keys := make([]uint16, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
