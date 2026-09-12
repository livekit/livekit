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

func makeEncoderFrame(t *testing.T, base uint16, count int) []rtp.Packet {
	t.Helper()
	packets := makeMediaPackets(t, base, count)
	for i := range packets {
		packets[i].Timestamp = uint32(base) * 3000
	}
	return packets
}

func TestEncoderDefaultsToNoProtection(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	p := rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT, Marker: true}
	for range 100 {
		p.SequenceNumber++
		require.Empty(t, e.Encode(&p, []byte{1, 2, 3}))
	}
	require.Zero(t, e.count)
	require.Nil(t, e.media, "negotiation alone must not allocate packet storage")
}

func TestEncoderFrameProtectionCounts(t *testing.T) {
	// Expected counts from upstream's 38/63/89 Q8 presets, including minimum-one
	// protection and rounding cases where a nominal percentage alone differs.
	for _, tc := range []struct{ packets, low, medium, high int }{
		{1, 1, 1, 1}, {2, 1, 1, 1}, {3, 1, 1, 1}, {5, 1, 1, 2},
		{6, 1, 1, 2}, {8, 1, 2, 3}, {20, 3, 5, 7}, {48, 7, 12, 17},
	} {
		for _, level := range []struct {
			percent uint32
			want    int
		}{
			{0, 0}, {15, tc.low}, {25, tc.medium}, {35, tc.high}, {100, tc.packets}, {101, tc.packets},
		} {
			t.Run(fmt.Sprintf("packets_%d/percent_%d", tc.packets, level.percent), func(t *testing.T) {
				e := NewEncoder(testFECPT, testFECSSRC, nil)
				e.SetProtectionPercent(level.percent)
				for i, p := range makeEncoderFrame(t, 100, tc.packets) {
					repairs := e.Encode(&p.Header, p.Payload)
					if i == tc.packets-1 {
						require.Len(t, repairs, level.want)
						for _, repair := range repairs {
							require.Equal(t, p.Timestamp, repair.Timestamp)
						}
					} else {
						require.Empty(t, repairs, "do not close the group before the frame ends")
					}
				}
				require.Zero(t, e.count)
			})
		}
	}
}

func TestEncoderSparseFrames(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(15)
	// No wall-clock wait is necessary: complete frames must not depend on the
	// previous frame's group age or on the arrival of a later frame.
	for i := range 3 {
		e.startedAt -= int64(10 * maxEncoderGroupAge)
		p := makeEncoderFrame(t, uint16(100+i), 1)[0]
		repairs := e.Encode(&p.Header, p.Payload)
		require.Len(t, repairs, 1, "even the first isolated frame needs immediate protection")
		decoder := newTestDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())
		recovered := decoder.DecodeFEC(&repairs[0])
		require.Len(t, recovered, 1)
		requirePacketEqual(t, &p, recovered[0])
	}
}

func TestEncoderState(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(20)
	e.SeedState(EncoderState{SSRC: testFECSSRC, NextSequenceNumber: 65535})
	for _, sn := range []uint16{65535, 0} {
		p := makeEncoderFrame(t, 100, 1)[0]
		repair := e.Encode(&p.Header, p.Payload)
		require.Len(t, repair, 1)
		require.Equal(t, sn, repair[0].SequenceNumber)
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
		media := append(makeEncoderFrame(t, base, 5), makeEncoderFrame(t, base+5, 5)...)
		e := NewEncoder(testFECPT, testFECSSRC, nil)
		e.SetProtectionPercent(20)
		e.sequenceNumber = 65535
		decoder := newTestDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())
		var repair []rtp.Packet
		for i := range media {
			media[i].CSRC = []uint32{123, 456}
			require.NoError(t, media[i].SetExtension(3, []byte{byte(i), 2, 3}))
			require.NoError(t, media[i].SetExtension(5, []byte{7, byte(i)}))
			packet := media[i].Clone()
			repair = append(repair, e.Encode(&packet.Header, packet.Payload)...)
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
			recovered := decoder.DecodeFEC(&repair[i])
			require.Len(t, recovered, 1)
			requirePacketEqual(t, &media[i*5+2], recovered[0])
		}
	}
}

func TestEncoderGroupBoundaries(t *testing.T) {
	for _, name := range []string{"gap", "duplicate", "out of order", "codec", "ssrc", "timestamp", "stale", "oversized", "padding", "empty"} {
		t.Run(name, func(t *testing.T) {
			e := NewEncoder(testFECPT, testFECSSRC, nil)
			e.SetProtectionPercent(20)
			media := makeEncoderFrame(t, 100, 5)
			for i := 0; i < 3; i++ {
				require.Empty(t, e.Encode(&media[i].Header, media[i].Payload))
			}
			p := &media[3]
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
			case "timestamp":
				p.Timestamp++
			case "stale":
				e.startedAt -= int64(maxEncoderGroupAge)
			case "oversized":
				p.Payload = make([]byte, maxEncoderMediaPacketSize)
			case "padding":
				p.Padding = true
			case "empty":
				p.Payload = nil
			}
			require.Empty(t, e.Encode(&p.Header, p.Payload))
			require.LessOrEqual(t, e.count, 1, "discard the previous partial block")
			fresh := makeEncoderFrame(t, 200, 1)[0]
			repairs := e.Encode(&fresh.Header, fresh.Payload)
			require.Len(t, repairs, 1)
			decoder := newTestDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())
			recovered := decoder.DecodeFEC(&repairs[0])
			require.Len(t, recovered, 1, "a missing marker must not mix consecutive frames")
			requirePacketEqual(t, &fresh, recovered[0])
		})
	}
}

func TestEncoderLargeFrame(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(35)
	media := makeEncoderFrame(t, 65510, 2*MaxMediaPacketsPerGroup+3)
	decoder := newTestDecoder(testFECSSRC, testMediaSSRC, logger.GetLogger())
	var repair []rtp.Packet
	for i := range media {
		out := e.Encode(&media[i].Header, media[i].Payload)
		switch i {
		case 47, 95:
			require.Len(t, out, 17)
		case 98:
			require.Len(t, out, 1, "protect the tail of the large frame")
		default:
			require.Empty(t, out)
		}
		require.Less(t, e.count, MaxMediaPacketsPerGroup)
		repair = append(repair, out...)
		if i != 47 && i != 95 && i != 98 {
			decoder.DecodeFEC(&media[i])
		}
	}
	var recovered []*rtp.Packet
	for i := range repair {
		recovered = append(recovered, decoder.DecodeFEC(&repair[i])...)
	}
	require.Len(t, recovered, 3)
	for i, lost := range []int{47, 95, 98} {
		requirePacketEqual(t, &media[lost], recovered[i])
	}
}

func TestEncoderSizeLimitAndClose(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(1) // one repair covering all three packet masks
	p := rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT}
	payload := make([]byte, maxEncoderMediaPacketSize-12)
	for i := range MaxMediaPacketsPerGroup {
		p.SequenceNumber++
		out := e.Encode(&p, payload)
		if i == MaxMediaPacketsPerGroup-1 {
			require.Len(t, out, 1)
			out[0].Extension = true
			out[0].ExtensionProfile = 0x1000 // RFC 8285 two-byte extensions
			require.NoError(t, out[0].SetExtension(20, []byte{1, 2, 3}))
			require.NoError(t, out[0].SetExtension(22, []byte{1, 2}))
			require.LessOrEqual(t, out[0].MarshalSize(), maxMediaPacketSize)
		}
	}
	e.Close()
	p.Marker = true
	for range 10 {
		p.SequenceNumber++
		require.Empty(t, e.Encode(&p, payload))
	}
	require.Nil(t, e.media)
}

func TestEncoderAccumulationReusesStorage(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(20)
	p := rtp.Header{Version: 2, SSRC: testMediaSSRC}
	require.NoError(t, p.SetExtension(3, []byte{1, 2, 3}))
	payload := make([]byte, 1200)
	require.Empty(t, e.Encode(&p, payload))
	require.NotNil(t, e.media.storage[0])
	require.Nil(t, e.media.storage[1], "small frames do not allocate large-frame payload storage")
	allocs := testing.AllocsPerRun(100, func() {
		e.count = 0
		for range MaxMediaPacketsPerGroup - 1 {
			p.SequenceNumber++
			e.Encode(&p, payload)
		}
	})
	require.Zero(t, allocs)
}

func TestEncoderProtectionPercentChanges(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(20)
	p := makeEncoderFrame(t, 100, 1)[0]
	first := e.Encode(&p.Header, p.Payload)
	require.Len(t, first, 1)
	p.Marker = false
	for range 3 {
		p.SequenceNumber++
		require.Empty(t, e.Encode(&p.Header, p.Payload))
	}
	e.SetProtectionPercent(40)
	p.SequenceNumber++
	p.Marker = true
	repairs := e.Encode(&p.Header, p.Payload)
	require.Len(t, repairs, 1, "discard the old partial frame on a setting change")
	require.Equal(t, first[0].SequenceNumber+1, repairs[0].SequenceNumber)
	e.SetProtectionPercent(0)
	require.Empty(t, e.Encode(&p.Header, p.Payload))
	require.Zero(t, e.OverheadPercent())
	e.SetProtectionPercent(40)
	p.Marker = false
	for range 4 {
		p.SequenceNumber++
		require.Empty(t, e.Encode(&p.Header, p.Payload))
	}
	e.SetProtectionPercent(40)
	p.SequenceNumber++
	p.Marker = true
	last := e.Encode(&p.Header, p.Payload)
	require.Len(t, last, 2, "unchanged settings retain the partial frame")
	require.Equal(t, repairs[0].SequenceNumber+1, last[0].SequenceNumber)
}

func TestEncoderMeasuredOverhead(t *testing.T) {
	var e *Encoder
	callbacks := 0
	e = NewEncoder(testFECPT, testFECSSRC, func(int, int) {
		e.GetState() // callbacks must be outside the encoder lock
		callbacks++
	})
	e.SetProtectionPercent(15)
	require.EqualValues(t, 15, e.OverheadPercent())
	p := rtp.Header{Version: 2, SSRC: testMediaSSRC, Marker: true}
	payload := make([]byte, 100)
	repairs := e.Encode(&p, payload)
	require.Len(t, repairs, 1)
	e.RecordSent(1, len(repairs[0].Payload), repairs[0].MarshalSize())
	require.EqualValues(t, 118, e.OverheadPercent(), "132 repair bytes / 112 media bytes, rounded up")
	p.Marker = false
	for i := range 20 {
		p.SequenceNumber++
		p.Marker = i == 19
		repairs = e.Encode(&p, payload)
	}
	require.Len(t, repairs, 3)
	e.overheadUpdatedAt -= int64(overheadUpdateInterval)
	e.RecordSent(3, 3*len(repairs[0].Payload), 3*repairs[0].MarshalSize())
	require.EqualValues(t, 19, e.OverheadPercent(), "adapt when frames become larger, including the longer FEC masks")
	require.Equal(t, 2, callbacks)
	e.SetProtectionPercent(25)
	require.EqualValues(t, 25, e.OverheadPercent(), "clear stale measurements on a preset change")
}

func TestEncoderConcurrentProtectionChangesAndClose(t *testing.T) {
	e := NewEncoder(testFECPT, testFECSSRC, nil)
	e.SetProtectionPercent(20)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 1000 {
			e.SetProtectionPercent(uint32(i % 101))
			e.OverheadPercent()
		}
	})
	wg.Go(func() {
		p := rtp.Header{Version: 2, SSRC: testMediaSSRC, Marker: true}
		for range 1000 {
			p.SequenceNumber++
			repair := e.Encode(&p, []byte{1, 2, 3})
			for _, packet := range repair {
				e.RecordSent(1, len(packet.Payload), packet.MarshalSize())
			}
		}
	})
	wg.Go(e.Close)
	wg.Wait()
}

func BenchmarkEncoder(b *testing.B) {
	for _, packets := range []int{1, 5, 20, MaxMediaPacketsPerGroup} {
		for _, percent := range []uint32{0, 15, 25, 35} {
			b.Run(fmt.Sprintf("packets_%d/percent_%d", packets, percent), func(b *testing.B) {
				e := NewEncoder(testFECPT, testFECSSRC, nil)
				e.SetProtectionPercent(percent)
				p := rtp.Header{Version: 2, SSRC: testMediaSSRC, PayloadType: testMediaPT}
				_ = p.SetExtension(3, []byte{1, 2, 3})
				payload := make([]byte, 1200)
				send := func() {
					p.Timestamp += 3000
					for i := range packets {
						p.SequenceNumber++
						p.Marker = i == packets-1
						e.Encode(&p, payload)
					}
				}
				send()
				b.ReportAllocs()
				b.SetBytes(int64(packets * len(payload)))
				b.ResetTimer()
				for b.Loop() {
					send()
				}
			})
		}
	}
}
