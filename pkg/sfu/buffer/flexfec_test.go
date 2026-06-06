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

// TestFlexFECRecoveryThroughFactory exercises the full SFU receive path: the source and
// repair buffers are created through the factory (as the SRTP layer would), associated
// via SetFECPair, and a lost source packet is recovered from the FlexFEC repair stream
// and forwarded out of the source buffer.
func TestFlexFECRecoveryThroughFactory(t *testing.T) {
	const (
		mediaSSRC = uint32(0xAABBCCDD)
		fecSSRC   = uint32(0x11223344)
		baseSeq   = uint16(5000)
		numMedia  = 10
		dropIdx   = 6
	)

	factory := NewFactoryOfBufferFactory(InitPacketBufferSizeVideo, InitPacketBufferSizeAudio).CreateBufferFactory()

	mediaBuf, ok := factory.GetOrNew(packetio.RTPBufferPacket, mediaSSRC).(*Buffer)
	require.True(t, ok)
	mediaBuf.codecType = webrtc.RTPCodecTypeAudio
	require.NoError(t, mediaBuf.Bind(
		webrtc.RTPParameters{Codecs: []webrtc.RTPCodecParameters{opusCodec}},
		opusCodec.RTPCodecCapability,
		0,
	))

	fecBuf, ok := factory.GetOrNew(packetio.RTPBufferPacket, fecSSRC).(*Buffer)
	require.True(t, ok)

	// associate the repair stream with the source stream (as a=ssrc-group:FEC-FR would)
	factory.SetFECPair(fecSSRC, mediaSSRC)
	require.NotNil(t, mediaBuf.fecDecoder)
	require.Equal(t, mediaBuf, fecBuf.primaryBufferForFEC)

	// build media packets and the protecting FlexFEC packet
	media := make([]rtp.Packet, numMedia)
	for i := 0; i < numMedia; i++ {
		payload := make([]byte, 16)
		for j := range payload {
			payload[j] = byte((i*11 + j*5 + 1) & 0xff)
		}
		media[i] = rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    uint8(opusCodec.PayloadType),
				SequenceNumber: baseSeq + uint16(i),
				Timestamp:      uint32(8000 + i*960),
				SSRC:           mediaSSRC,
			},
			Payload: payload,
		}
	}

	encoder := flexfec.NewFlexEncoder03(uint8(49), fecSSRC)
	mediaForFec := make([]rtp.Packet, numMedia)
	copy(mediaForFec, media)
	fecPackets := encoder.EncodeFec(mediaForFec, 1)
	require.NotEmpty(t, fecPackets)

	// collect forwarded packets out of the source buffer
	got := make(chan *ExtPacket, numMedia*2)
	go func() {
		var buf [1500]byte
		for {
			ep, err := mediaBuf.ReadExtended(buf[:])
			if err != nil {
				return
			}
			if ep != nil {
				clone := *ep
				got <- &clone
			}
		}
	}()

	// deliver all source packets except the dropped one
	for i := 0; i < numMedia; i++ {
		if i == dropIdx {
			continue
		}
		raw, err := media[i].Marshal()
		require.NoError(t, err)
		_, err = mediaBuf.Write(raw)
		require.NoError(t, err)
	}

	// deliver the repair packet to the FEC buffer -> triggers recovery + injection
	for _, fp := range fecPackets {
		raw, err := fp.Marshal()
		require.NoError(t, err)
		_, err = fecBuf.Write(raw)
		require.NoError(t, err)
	}

	// the dropped source packet must be recovered and forwarded
	recoveredSeq := baseSeq + uint16(dropIdx)
	deadline := time.After(2 * time.Second)
	seen := map[uint16]*ExtPacket{}
	for {
		select {
		case ep := <-got:
			seen[ep.Packet.SequenceNumber] = ep
			if rec, found := seen[recoveredSeq]; found {
				require.Equal(t, mediaSSRC, rec.Packet.SSRC)
				require.Equal(t, media[dropIdx].Payload, rec.Packet.Payload)
				require.Equal(t, media[dropIdx].Timestamp, rec.Packet.Timestamp)
				_ = mediaBuf.Close()
				return
			}
		case <-deadline:
			_ = mediaBuf.Close()
			t.Fatalf("recovered packet seq %d not forwarded; saw %v", recoveredSeq, seqKeys(seen))
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
