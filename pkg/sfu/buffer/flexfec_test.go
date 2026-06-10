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
	"fmt"
	"testing"

	pionflexfec "github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"
	"github.com/pion/transport/v4/packetio"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/logger"
)

const (
	testFECMediaSSRC = uint32(0x12345678)
	testFECSSRC      = uint32(0x23456789)
	testFECPT        = uint8(49)
)

// fecPacketStore mimics the primary buffer's packet bucket keyed by extended
// wire sequence number
type fecPacketStore struct {
	packets map[uint64][]byte
	highest uint64
	hasAny  bool
	window  uint64
}

func newFECPacketStore(window int) *fecPacketStore {
	return &fecPacketStore{
		packets: make(map[uint64][]byte),
		window:  uint64(window),
	}
}

func (s *fecPacketStore) add(esn uint64, raw []byte) {
	s.packets[esn] = raw
	if !s.hasAny || esn > s.highest {
		s.highest = esn
	}
	s.hasAny = true
}

func (s *fecPacketStore) getPacket(buf []byte, esn uint64) (int, error) {
	if !s.hasAny || esn > s.highest {
		return 0, bucket.ErrPacketTooNew
	}
	if s.highest-esn >= s.window {
		return 0, bucket.ErrPacketTooOld
	}
	raw, ok := s.packets[esn]
	if !ok {
		return 0, bucket.ErrPacketSizeInvalid
	}
	return copy(buf, raw), nil
}

func (s *fecPacketStore) decoderParams() flexFECDecoderParams {
	return flexFECDecoderParams{
		Logger:       logger.GetLogger(),
		SSRC:         testFECMediaSSRC,
		GetPacket:    s.getPacket,
		ExtHighestSN: func() (uint64, bool) { return s.highest, s.hasAny },
		WindowSize:   func() int { return int(s.window) },
	}
}

// makeFECMediaPackets builds count consecutive media packets starting at baseSN
// with varying payload sizes, header extensions and marker bits
func makeFECMediaPackets(t *testing.T, baseSN uint16, count int) ([]rtp.Packet, [][]byte) {
	t.Helper()

	packets := make([]rtp.Packet, 0, count)
	raws := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: baseSN + uint16(i),
				Timestamp:      0x1000 + uint32(i/3)*3000,
				SSRC:           testFECMediaSSRC,
				Marker:         i%3 == 2,
			},
			Payload: make([]byte, 20+(i*37)%600),
		}
		for j := range pkt.Payload {
			pkt.Payload[j] = byte(i + j)
		}
		if i%3 == 0 {
			require.NoError(t, pkt.Header.SetExtension(3, []byte{byte(i), byte(i >> 8)}))
		}

		raw, err := pkt.Marshal()
		require.NoError(t, err)
		packets = append(packets, pkt)
		raws = append(raws, raw)
	}
	return packets, raws
}

func encodeFECPackets(t *testing.T, media []rtp.Packet, numFEC uint32) []rtp.Packet {
	t.Helper()

	fecPackets := pionflexfec.NewFlexEncoder03(testFECPT, testFECSSRC).EncodeFec(media, numFEC)
	require.NotEmpty(t, fecPackets)
	return fecPackets
}

func TestParseFlexFEC03Header(t *testing.T) {
	buildPayload := func(k0, k1, k2 bool, size int) []byte {
		payload := make([]byte, size)
		payload[8] = 1 // SSRCCount
		// protected SSRC
		payload[12], payload[13], payload[14], payload[15] = 0x12, 0x34, 0x56, 0x78
		// SN base
		payload[16], payload[17] = 0x10, 0x01
		if k0 {
			payload[18] |= 0x80
		}
		if k1 && size > 20 {
			payload[20] |= 0x80
		}
		if k2 && size > 24 {
			payload[24] |= 0x80
		}
		return payload
	}

	t.Run("mask sizes", func(t *testing.T) {
		// k-bit 0 set, 15 bit mask, offsets 0 and 14
		payload := buildPayload(true, false, false, 30)
		payload[18] |= 0x40 // offset 0
		payload[19] |= 0x01 // offset 14
		h, err := parseFlexFEC03Header(payload)
		require.NoError(t, err)
		require.Equal(t, uint32(0x12345678), h.protectedSSRC)
		require.Equal(t, uint16(0x1001), h.snBase)
		require.Equal(t, []uint16{0, 14}, h.offsets)
		require.Equal(t, 20, h.payloadOffset)

		// k-bit 1 set, 15+31 bit masks, offsets 15 and 45
		payload = buildPayload(false, true, false, 40)
		payload[20] |= 0x40 // offset 15
		payload[23] |= 0x01 // offset 45
		h, err = parseFlexFEC03Header(payload)
		require.NoError(t, err)
		require.Equal(t, []uint16{15, 45}, h.offsets)
		require.Equal(t, 24, h.payloadOffset)

		// k-bit 2 set, 15+31+63 bit masks, offsets 46 and 108
		payload = buildPayload(false, false, true, 40)
		payload[24] |= 0x40 // offset 46
		payload[31] |= 0x01 // offset 108
		h, err = parseFlexFEC03Header(payload)
		require.NoError(t, err)
		require.Equal(t, []uint16{46, 108}, h.offsets)
		require.Equal(t, 32, h.payloadOffset)
	})

	t.Run("rejects", func(t *testing.T) {
		// truncated
		_, err := parseFlexFEC03Header(make([]byte, 19))
		require.ErrorIs(t, err, errFECPacketTruncated)

		// truncated with k-bit 0 unset
		_, err = parseFlexFEC03Header(buildPayload(false, true, false, 20))
		require.ErrorIs(t, err, errFECPacketTruncated)

		// truncated with k-bits 0 and 1 unset
		_, err = parseFlexFEC03Header(buildPayload(false, false, true, 24))
		require.ErrorIs(t, err, errFECPacketTruncated)

		// retransmission bit
		payload := buildPayload(true, false, false, 30)
		payload[0] |= 0x80
		_, err = parseFlexFEC03Header(payload)
		require.ErrorIs(t, err, errFECRetransmissionBit)

		// inflexible mask bit
		payload = buildPayload(true, false, false, 30)
		payload[0] |= 0x40
		_, err = parseFlexFEC03Header(payload)
		require.ErrorIs(t, err, errFECInflexibleMask)

		// multiple SSRC
		payload = buildPayload(true, false, false, 30)
		payload[8] = 2
		_, err = parseFlexFEC03Header(payload)
		require.ErrorIs(t, err, errFECMultipleSSRC)

		// k-bit of last mask not set
		payload = buildPayload(false, false, false, 40)
		_, err = parseFlexFEC03Header(payload)
		require.ErrorIs(t, err, errFECLastMaskKBitNotSet)

		// empty mask
		payload = buildPayload(true, false, false, 30)
		_, err = parseFlexFEC03Header(payload)
		require.ErrorIs(t, err, errFECEmptyMask)
	})
}

func TestFlexFECDecoderRoundTrip(t *testing.T) {
	for _, blockSize := range []int{10, 30, 100} {
		for _, dropIdx := range []int{0, blockSize / 2, blockSize - 1} {
			t.Run(fmt.Sprintf("block=%d,drop=%d", blockSize, dropIdx), func(t *testing.T) {
				media, raws := makeFECMediaPackets(t, 100, blockSize)
				fecPkt := encodeFECPackets(t, media, 1)[0]

				store := newFECPacketStore(1000)
				for i, raw := range raws {
					if i != dropIdx {
						store.add(uint64(media[i].SequenceNumber), raw)
					}
				}

				d := newFlexFECDecoder(store.decoderParams())
				recovered := d.AddFEC(fecPkt.SequenceNumber, fecPkt.Payload, 0)

				if dropIdx == blockSize-1 {
					// the dropped packet is past the highest received one, it
					// is not detectable as lost until the stream advances
					require.Empty(t, recovered)
					next, nextRaws := makeFECMediaPackets(t, 100+uint16(blockSize), 1)
					store.add(uint64(next[0].SequenceNumber), nextRaws[0])
					recovered = d.OnMediaPacket(next[0].SequenceNumber)
				}

				require.Len(t, recovered, 1)
				require.Equal(t, raws[dropIdx], recovered[0])

				stats := d.Stats()
				require.Equal(t, uint32(1), stats.PacketsReceived)
				require.Equal(t, uint32(1), stats.RecoveryAttempts)
				require.Equal(t, uint32(1), stats.PacketsRecovered)
				require.Equal(t, uint32(0), stats.RecoveryFailed)
				require.False(t, d.HasPending())
			})
		}
	}
}

func TestFlexFECDecoderNoLoss(t *testing.T) {
	media, raws := makeFECMediaPackets(t, 5000, 12)
	fecPkt := encodeFECPackets(t, media, 1)[0]

	store := newFECPacketStore(1000)
	for i, raw := range raws {
		store.add(uint64(media[i].SequenceNumber), raw)
	}

	d := newFlexFECDecoder(store.decoderParams())
	require.Empty(t, d.AddFEC(fecPkt.SequenceNumber, fecPkt.Payload, 0))

	stats := d.Stats()
	require.Equal(t, uint32(1), stats.PacketsReceived)
	require.Equal(t, uint32(1), stats.PacketsUnused)
	require.Equal(t, uint32(0), stats.RecoveryAttempts)
	require.False(t, d.HasPending())
}

func TestFlexFECDecoderFECBeforeMedia(t *testing.T) {
	media, raws := makeFECMediaPackets(t, 300, 10)
	fecPkt := encodeFECPackets(t, media, 1)[0]

	store := newFECPacketStore(1000)
	d := newFlexFECDecoder(store.decoderParams())

	// FEC arrives before any media
	require.Empty(t, d.AddFEC(fecPkt.SequenceNumber, fecPkt.Payload, 0))
	require.True(t, d.HasPending())

	// media arrives, packet 4 is lost
	var recovered [][]byte
	for i, raw := range raws {
		if i == 4 {
			continue
		}
		store.add(uint64(media[i].SequenceNumber), raw)
		recovered = append(recovered, d.OnMediaPacket(media[i].SequenceNumber)...)
	}

	require.Len(t, recovered, 1)
	require.Equal(t, raws[4], recovered[0])
	require.False(t, d.HasPending())
}

func TestFlexFECDecoderTwoMissing(t *testing.T) {
	media, raws := makeFECMediaPackets(t, 700, 10)
	fecPkt := encodeFECPackets(t, media, 1)[0]

	store := newFECPacketStore(1000)
	for i, raw := range raws {
		if i == 2 || i == 6 {
			continue
		}
		store.add(uint64(media[i].SequenceNumber), raw)
	}

	d := newFlexFECDecoder(store.decoderParams())
	require.Empty(t, d.AddFEC(fecPkt.SequenceNumber, fecPkt.Payload, 0))
	require.True(t, d.HasPending())
	require.Equal(t, uint32(0), d.Stats().RecoveryAttempts)

	// packet 2 arrives late, e.g. via RTX, packet 6 becomes recoverable
	store.add(uint64(media[2].SequenceNumber), raws[2])
	recovered := d.OnMediaPacket(media[2].SequenceNumber)
	require.Len(t, recovered, 1)
	require.Equal(t, raws[6], recovered[0])
	require.False(t, d.HasPending())
}

func TestFlexFECDecoderDuplicateFEC(t *testing.T) {
	media, raws := makeFECMediaPackets(t, 900, 10)
	fecPkt := encodeFECPackets(t, media, 1)[0]

	store := newFECPacketStore(1000)
	for i, raw := range raws {
		if i == 3 || i == 5 {
			continue
		}
		store.add(uint64(media[i].SequenceNumber), raw)
	}

	d := newFlexFECDecoder(store.decoderParams())
	require.Empty(t, d.AddFEC(fecPkt.SequenceNumber, fecPkt.Payload, 0))
	require.Empty(t, d.AddFEC(fecPkt.SequenceNumber, fecPkt.Payload, 0))

	stats := d.Stats()
	require.Equal(t, uint32(2), stats.PacketsReceived)
	require.Equal(t, 1, len(d.pending), "duplicate must not be buffered twice")
}

func TestFlexFECDecoderInvalid(t *testing.T) {
	store := newFECPacketStore(1000)
	d := newFlexFECDecoder(store.decoderParams())

	// garbage payload
	require.Empty(t, d.AddFEC(1, make([]byte, 10), 0))

	// valid header protecting another SSRC
	media, _ := makeFECMediaPackets(t, 100, 5)
	for i := range media {
		media[i].SSRC = testFECMediaSSRC + 1
	}
	fecPkt := encodeFECPackets(t, media, 1)[0]
	require.Empty(t, d.AddFEC(fecPkt.SequenceNumber, fecPkt.Payload, 0))

	stats := d.Stats()
	require.Equal(t, uint32(2), stats.PacketsReceived)
	require.Equal(t, uint32(2), stats.PacketsInvalid)
	require.False(t, d.HasPending())
}

func TestFlexFECDecoderWraparound(t *testing.T) {
	// media packets crossing the 16-bit sequence number wrap
	media, raws := makeFECMediaPackets(t, 65530, 10)
	fecPkt := encodeFECPackets(t, media, 1)[0]

	const cycle = uint64(7) << 16
	esn := func(i int) uint64 {
		// 65530..65535 are in the cycle, 0..3 in the next
		if media[i].SequenceNumber >= 65530 {
			return cycle | uint64(media[i].SequenceNumber)
		}
		return cycle + (1 << 16) + uint64(media[i].SequenceNumber)
	}

	for _, dropIdx := range []int{2, 8} { // one drop on each side of the wrap
		t.Run(fmt.Sprintf("drop=%d", dropIdx), func(t *testing.T) {
			store := newFECPacketStore(1000)
			for i, raw := range raws {
				if i != dropIdx {
					store.add(esn(i), raw)
				}
			}

			d := newFlexFECDecoder(store.decoderParams())
			recovered := d.AddFEC(fecPkt.SequenceNumber, fecPkt.Payload, 0)
			require.Len(t, recovered, 1)
			require.Equal(t, raws[dropIdx], recovered[0])
		})
	}
}

func TestFlexFECDecoderOverlappingChain(t *testing.T) {
	// two FEC packets with overlapping coverage, recovering a packet via the
	// first enables the second once the recovered packet is fed back
	media, raws := makeFECMediaPackets(t, 2000, 12)
	fecA := encodeFECPackets(t, media[:8], 1)[0] // protects 0..7
	fecB := encodeFECPackets(t, media[4:], 1)[0] // protects 4..11

	store := newFECPacketStore(1000)
	for i, raw := range raws {
		if i == 6 || i == 10 {
			continue
		}
		store.add(uint64(media[i].SequenceNumber), raw)
	}

	d := newFlexFECDecoder(store.decoderParams())

	// distinct FEC stream sequence numbers, each encoder starts at the same one
	// B cannot recover, 6 and 10 missing
	require.Empty(t, d.AddFEC(1, fecB.Payload, 0))
	// A recovers 6
	recovered := d.AddFEC(2, fecA.Payload, 0)
	require.Len(t, recovered, 1)
	require.Equal(t, raws[6], recovered[0])

	// the buffer injects the recovered packet and notifies the decoder,
	// which lets B recover 10
	store.add(uint64(media[6].SequenceNumber), raws[6])
	recovered = d.OnMediaPacket(media[6].SequenceNumber)
	require.Len(t, recovered, 1)
	require.Equal(t, raws[10], recovered[0])

	require.False(t, d.HasPending())
	require.Equal(t, uint32(2), d.Stats().PacketsRecovered)
}

func TestFlexFECDecoderStaleEviction(t *testing.T) {
	media, raws := makeFECMediaPackets(t, 100, 10)
	fecPkt := encodeFECPackets(t, media, 1)[0]

	store := newFECPacketStore(50)
	for i, raw := range raws {
		if i == 2 || i == 6 {
			continue
		}
		store.add(uint64(media[i].SequenceNumber), raw)
	}

	d := newFlexFECDecoder(store.decoderParams())
	require.Empty(t, d.AddFEC(1, fecPkt.Payload, 0))
	require.True(t, d.HasPending())

	// the stream advances beyond the window, next FEC packet triggers a sweep
	next, nextRaws := makeFECMediaPackets(t, 300, 10)
	for i, raw := range nextRaws {
		store.add(uint64(next[i].SequenceNumber), raw)
	}
	nextFEC := encodeFECPackets(t, next, 1)[0]
	require.Empty(t, d.AddFEC(2, nextFEC.Payload, 0))

	stats := d.Stats()
	require.Equal(t, uint32(1), stats.RecoveryFailed, "stale FEC with missing packets evicted as failed")
	require.Equal(t, uint32(1), stats.PacketsUnused, "fresh FEC with no losses evicted as unused")
	require.False(t, d.HasPending())
}

// TestBufferFlexFECIntegration exercises the full path, a FEC packet written to
// the paired FEC buffer recovers a lost media packet into the primary's bucket
func TestBufferFlexFECIntegration(t *testing.T) {
	flexfecCodec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeFlexFEC03,
			ClockRate:   90000,
			SDPFmtpLine: "repair-window=10000000",
		},
		PayloadType: 49,
	}

	media, raws := makeFECMediaPackets(t, 100, 10)
	fecPkt := encodeFECPackets(t, media, 1)[0]
	fecRaw, err := fecPkt.Marshal()
	require.NoError(t, err)

	for _, pairFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("pairBeforeFECBuffer=%v", pairFirst), func(t *testing.T) {
			factory := NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()
			primary := factory.GetOrNew(packetio.RTPBufferPacket, testFECMediaSSRC).(*Buffer)
			require.NoError(t, primary.Bind(webrtc.RTPParameters{
				Codecs: []webrtc.RTPCodecParameters{vp8Codec, flexfecCodec},
			}, vp8Codec.RTPCodecCapability, 0))
			require.Equal(t, uint8(49), primary.fecPayloadType, "fec payload type learned at bind")

			twccResponder := twcc.NewTransportWideCCResponder()
			primary.SetTWCCAndExtID(twccResponder, 5)

			// media packets arrive, packet 4 is lost
			for i, raw := range raws {
				if i == 4 {
					continue
				}
				_, err := primary.Write(raw)
				require.NoError(t, err)
			}

			if pairFirst {
				factory.SetFECPair(testFECSSRC, testFECMediaSSRC)
			}
			fecBuffer := factory.GetOrNew(packetio.RTPBufferPacket, testFECSSRC).(*Buffer)
			if !pairFirst {
				factory.SetFECPair(testFECSSRC, testFECMediaSSRC)
			}

			_, err = fecBuffer.Write(fecRaw)
			require.NoError(t, err)

			// the recovered packet must be byte equal in the primary's bucket
			primary.Lock()
			ref, ok := primary.extHighestWireSNLocked()
			require.True(t, ok)
			buf := make([]byte, bucket.RTPMaxPktSize)
			n, getErr := primary.getWirePacketLocked(buf, unwrapNearESN(media[4].SequenceNumber, ref))
			primary.Unlock()
			require.NoError(t, getErr)
			require.Equal(t, raws[4], buf[:n])

			stats, hasStats := primary.GetFECStreamStats()
			require.True(t, hasStats)
			require.Equal(t, uint32(1), stats.PacketsReceived)
			require.Equal(t, uint32(1), stats.RecoveryAttempts)
			require.Equal(t, uint32(1), stats.PacketsRecovered)
		})
	}
}

// TestBufferFlexFECBeforePairing verifies FEC packets queued in the unbound FEC
// buffer are processed once the pairing is established
func TestBufferFlexFECBeforePairing(t *testing.T) {
	media, raws := makeFECMediaPackets(t, 100, 10)
	fecPkt := encodeFECPackets(t, media, 1)[0]
	fecRaw, err := fecPkt.Marshal()
	require.NoError(t, err)

	factory := NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()
	primary := factory.GetOrNew(packetio.RTPBufferPacket, testFECMediaSSRC).(*Buffer)
	require.NoError(t, primary.Bind(webrtc.RTPParameters{
		Codecs: []webrtc.RTPCodecParameters{vp8Codec},
	}, vp8Codec.RTPCodecCapability, 0))
	require.Equal(t, uint8(0), primary.fecPayloadType, "no flexfec codec negotiated")

	// FEC packet arrives before the pairing is known, queued in the FEC buffer
	fecBuffer := factory.GetOrNew(packetio.RTPBufferPacket, testFECSSRC).(*Buffer)
	_, err = fecBuffer.Write(fecRaw)
	require.NoError(t, err)

	for i, raw := range raws {
		if i == 4 {
			continue
		}
		_, err := primary.Write(raw)
		require.NoError(t, err)
	}

	// pairing flushes the queued FEC packet into the primary
	factory.SetFECPair(testFECSSRC, testFECMediaSSRC)

	stats, hasStats := primary.GetFECStreamStats()
	require.True(t, hasStats)
	require.Equal(t, uint32(1), stats.PacketsReceived)
	require.Equal(t, uint32(1), stats.PacketsRecovered)
}

func TestFlexFECDecoderPendingOverflow(t *testing.T) {
	store := newFECPacketStore(100000)
	params := store.decoderParams()
	params.MaxPendingFEC = 4
	d := newFlexFECDecoder(params)

	// no media received, FEC packets pile up
	for i := 0; i < 6; i++ {
		media, _ := makeFECMediaPackets(t, uint16(1000+i*20), 5)
		fecPkt := encodeFECPackets(t, media, 1)[0]
		require.Empty(t, d.AddFEC(fecPkt.SequenceNumber+uint16(i), fecPkt.Payload, 0))
	}

	require.Equal(t, 4, len(d.pending))
	require.Equal(t, uint32(2), d.Stats().PacketsDiscardedOld)
}
