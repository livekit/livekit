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
	"fmt"
	"sync"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
)

func TestEncoderDefaultsToNoProtection(t *testing.T) {
	encoder := NewEncoder(testFECPT, testFECSSRC, nil)
	p := rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT}
	for range 100 {
		p.SequenceNumber++
		require.Empty(t, encoder.Encode(&p, []byte{1, 2, 3}))
	}
	require.Zero(t, encoder.count, "disabled protection must not retain media")
	require.Nil(t, encoder.media, "negotiation alone must not allocate packet storage")
}

func TestEncoderState(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(20)
	e.SeedState(EncoderState{SSRC: testFECSSRC, NextSequenceNumber: 65535})
	for _, sn := range []uint16{65535, 0} {
		media := makeMediaPackets(t, 100, MediaPacketsPerGroup)
		for i := range media {
			repair := e.Encode(&media[i].Header, media[i].Payload)
			if i == MediaPacketsPerGroup-1 {
				require.Len(t, repair, 1)
				require.Equal(t, sn, repair[0].SequenceNumber)
			}
		}
		e.Close()
		state := e.GetState()
		require.Equal(t, sn+1, state.NextSequenceNumber)
		e = NewEncoder(testFECPT, testFECSSRC, nil)
		e.SeedState(state)
		e.SetProtectionPercent(20)
	}
	state := e.GetState()
	e.SeedState(EncoderState{SSRC: testFECSSRC + 1, NextSequenceNumber: state.NextSequenceNumber + 100})
	require.Equal(t, state, e.GetState(), "do not seed a different repair SSRC")
}

func TestEncoderRecoveryWithReusedMemory(t *testing.T) {
	for _, base := range []uint16{100, 65533} {
		media := makeMediaPackets(t, base, 2*MediaPacketsPerGroup)
		encoder := NewEncoder(testFECPT, testFECSSRC, nil)
		encoder.SetProtectionPercent(20)
		encoder.sequenceNumber = 65535
		decoder := newTestDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())
		var repair []rtp.Packet
		for i := range media {
			media[i].CSRC = []uint32{123, 456}
			require.NoError(t, media[i].SetExtension(3, []byte{byte(i), 2, 3}))
			require.NoError(t, media[i].SetExtension(5, []byte{7, byte(i)}))
			packet := media[i].Clone()
			repair = append(repair, encoder.Encode(&packet.Header, packet.Payload)...)
			// The caller owns and immediately recycles every part of the input.
			clear(packet.Payload)
			clear(packet.CSRC)
			clear(packet.GetExtension(3))
			packet.Header = rtp.Header{}
			if i != 2 && i != 7 {
				decoder.DecodeFEC(&media[i])
			}
		}
		require.Len(t, repair, 2)
		require.Equal(t, uint16(65535), repair[0].SequenceNumber)
		require.Zero(t, repair[1].SequenceNumber)
		for i := range repair {
			require.Equal(t, media[(i+1)*MediaPacketsPerGroup-1].Timestamp, repair[i].Timestamp)
			recovered := decoder.DecodeFEC(&repair[i])
			require.Len(t, recovered, 1)
			expected, err := media[i*MediaPacketsPerGroup+2].Marshal()
			require.NoError(t, err)
			actual, err := recovered[0].Marshal()
			require.NoError(t, err)
			require.Equal(t, expected, actual)
		}
	}
}

func TestEncoderGroupBoundaries(t *testing.T) {
	for _, name := range []string{"gap", "duplicate", "out of order", "codec", "ssrc", "stale", "oversized", "padding", "empty"} {
		t.Run(name, func(t *testing.T) {
			e := NewEncoder(testFECPT, testFECSSRC, nil)
			e.SetProtectionPercent(20)
			media := makeMediaPackets(t, 100, 10)
			for i := 0; i < 4; i++ {
				require.Empty(t, e.Encode(&media[i].Header, media[i].Payload))
			}
			p := &media[4]
			switch name {
			case "gap":
				p.SequenceNumber++
			case "duplicate":
				p.SequenceNumber--
			case "out of order":
				p.SequenceNumber -= 2
			case "codec":
				p.PayloadType++
			case "ssrc":
				p.SSRC++
			case "stale":
				e.startedAt -= int64(maxEncoderGroupAge)
			case "oversized":
				p.Payload = make([]byte, maxEncoderMediaPacketSize)
			case "padding":
				p.Padding = true
			case "empty":
				p.Payload = nil
			}
			require.Empty(t, e.Encode(&p.Header, p.Payload), "must not protect a discontinuous group")
			fresh := makeMediaPackets(t, 200, MediaPacketsPerGroup)
			for i := range fresh {
				out := e.Encode(&fresh[i].Header, fresh[i].Payload)
				if i == MediaPacketsPerGroup-1 {
					require.Len(t, out, 1, "resume protection after the discontinuity")
				} else {
					require.Empty(t, out)
				}
			}
		})
	}
}

func TestEncoderSizeLimitAndClose(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(20)
	p := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT}, Payload: make([]byte, maxEncoderMediaPacketSize-12)}
	for i := range MediaPacketsPerGroup {
		p.SequenceNumber++
		out := e.Encode(&p.Header, p.Payload)
		if i == MediaPacketsPerGroup-1 {
			require.Len(t, out, 1)
			require.NoError(t, out[0].SetExtension(3, []byte{1, 2, 3}))
			require.NoError(t, out[0].SetExtension(5, []byte{1, 2}))
			require.LessOrEqual(t, out[0].MarshalSize(), maxMediaPacketSize)
		}
	}
	e.Close()
	for range 2 * MediaPacketsPerGroup {
		p.SequenceNumber++
		require.Empty(t, e.Encode(&p.Header, p.Payload))
	}
}

func TestEncoderConcurrentClose(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(20)
	var wg sync.WaitGroup
	for n := range 4 {
		wg.Go(func() {
			p := rtp.Header{Version: 2, SSRC: testMediaSSRC, SequenceNumber: uint16(n * 1000)}
			for range 100 {
				e.Encode(&p, []byte{1, 2, 3})
				p.SequenceNumber++
			}
		})
	}
	e.Close()
	wg.Wait()
}

func TestEncoderAccumulationReusesStorage(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(20)
	p := rtp.Header{Version: 2, SSRC: testMediaSSRC}
	require.NoError(t, p.SetExtension(3, []byte{1, 2, 3}))
	payload := make([]byte, 1200)
	allocs := testing.AllocsPerRun(100, func() {
		e.count = 0
		for range MediaPacketsPerGroup - 1 {
			p.SequenceNumber++
			e.Encode(&p, payload)
		}
	})
	require.Zero(t, allocs)
}

func BenchmarkEncoder(b *testing.B) {
	for _, percent := range []uint32{0, 15, 25, 35} {
		b.Run(fmt.Sprintf("percent_%d", percent), func(b *testing.B) {
			e := NewEncoder(testFECPT, testFECSSRC, nil)
			e.SetProtectionPercent(percent)
			p := rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT}
			_ = p.SetExtension(3, []byte{1, 2, 3})
			payload := make([]byte, 1200)
			// Warm the reusable packet slots and Pion's coverage cache.
			for range 2 * MediaPacketsPerGroup {
				p.SequenceNumber++
				e.Encode(&p, payload)
			}
			b.ReportAllocs()
			b.SetBytes(int64(MediaPacketsPerGroup * len(payload)))
			b.ResetTimer()
			for b.Loop() {
				for range MediaPacketsPerGroup {
					p.SequenceNumber++
					e.Encode(&p, payload)
				}
			}
		})
	}
}

func TestEncoderProtectionPercent(t *testing.T) {
	for percent := uint32(0); percent <= 101; percent++ {
		t.Run(fmt.Sprint(percent), func(t *testing.T) {
			encoder := NewEncoder(testFECPT, testFECSSRC, nil)
			encoder.SetProtectionPercent(percent)
			p := rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT}
			var count int
			var previous uint16
			for range 100 {
				p.SequenceNumber++
				repairs := encoder.Encode(&p, []byte{1, 2, 3})
				require.LessOrEqual(t, len(repairs), MediaPacketsPerGroup)
				for _, repair := range repairs {
					if count > 0 {
						require.Equal(t, previous+1, repair.SequenceNumber)
					}
					count++
					previous = repair.SequenceNumber
				}
			}
			require.EqualValues(t, min(percent, MaxProtectionPercent), count)
		})
	}
}

func TestEncoderProtectionPercentChanges(t *testing.T) {
	encoder := NewEncoder(testFECPT, testFECSSRC, nil)
	encoder.SetProtectionPercent(20)
	p := rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT}
	send := func(count int) []rtp.Packet {
		var result []rtp.Packet
		for range count {
			p.SequenceNumber++
			result = append(result, encoder.Encode(&p, []byte{1, 2, 3})...)
		}
		return result
	}
	first := send(MediaPacketsPerGroup)
	require.Len(t, first, 1)
	require.Empty(t, send(4))
	encoder.SetProtectionPercent(40)
	require.Empty(t, send(4), "discard the old partial group")
	repairs := send(1)
	require.Len(t, repairs, 2)
	require.Equal(t, first[0].SequenceNumber+1, repairs[0].SequenceNumber)
	encoder.SetProtectionPercent(0)
	require.Empty(t, send(100))
	require.Zero(t, encoder.count)
	encoder.SetProtectionPercent(10)
	require.Empty(t, send(5))
	encoder.SetProtectionPercent(10) // unchanged settings must retain fractional credit
	last := send(5)
	require.Len(t, last, 1)
	require.Equal(t, repairs[1].SequenceNumber+1, last[0].SequenceNumber)
}

func TestEncoderMultipleRepairRecovery(t *testing.T) {
	encoder := NewEncoder(testFECPT, testFECSSRC, nil)
	encoder.SetProtectionPercent(60)
	media := makeMediaPackets(t, 100, MediaPacketsPerGroup)
	decoder := newTestDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())
	var repair []rtp.Packet
	for i := range media {
		repair = append(repair, encoder.Encode(&media[i].Header, media[i].Payload)...)
		if i >= 3 {
			decoder.DecodeFEC(&media[i])
		}
	}
	require.Len(t, repair, 3)
	for i := range repair {
		recovered := decoder.DecodeFEC(&repair[i])
		require.Len(t, recovered, 1)
		requirePacketEqual(t, &media[i], recovered[0])
	}
}

func TestEncoderConcurrentProtectionChanges(t *testing.T) {
	encoder := NewEncoder(testFECPT, testFECSSRC, nil)
	encoder.SetProtectionPercent(20)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 1000 {
			encoder.SetProtectionPercent(uint32(i % 101))
		}
	})
	wg.Go(func() {
		p := rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT}
		for range 1000 {
			p.SequenceNumber++
			encoder.Encode(&p, []byte{1, 2, 3})
		}
	})
	wg.Wait()
	encoder.Close()
}
