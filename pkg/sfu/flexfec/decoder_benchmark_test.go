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
	"testing"

	pionflexfec "github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"

	"github.com/livekit/protocol/logger"
)

func benchmarkMediaPackets(count, payloadSize int) []rtp.Packet {
	packets := make([]rtp.Packet, count)
	for i := range packets {
		packets[i] = rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    testMediaPT,
				SequenceNumber: 100 + uint16(i),
				Timestamp:      3000 * uint32(i),
				SSRC:           testMediaSSRC,
			},
			Payload: make([]byte, payloadSize),
		}
	}
	return packets
}

func benchmarkMediaLookup(media []rtp.Packet, missingIndex int) MediaPacketLookup {
	packets := make(map[uint16][]byte, len(media))
	for i := range media {
		if i == missingIndex {
			continue
		}
		raw, err := media[i].Marshal()
		if err != nil {
			panic(err)
		}
		packets[media[i].SequenceNumber] = raw
	}

	return func(sequenceNumber uint16, dst []byte) (int, error) {
		packet, ok := packets[sequenceNumber]
		if !ok {
			return 0, errTestMediaPacketNotFound
		}
		return copy(dst, packet), nil
	}
}

func BenchmarkDecoderMediaSteadyState1200(b *testing.B) {
	decoder := NewDecoder(testFECSSRC, testMediaSSRC, nil, logger.GetLogger())
	packet := benchmarkMediaPackets(1, 1200)[0]

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		packet.SequenceNumber = uint16(i)
		packet.Timestamp = uint32(i) * 3000
		decoder.DecodeFEC(&packet)
	}
}

func BenchmarkDecoderCompleteWindow10x1200(b *testing.B) {
	media := benchmarkMediaPackets(10, 1200)
	fecPackets := pionflexfec.NewFlexEncoder03(testFECPT, testFECSSRC).EncodeFec(media, 1)
	lookup := benchmarkMediaLookup(media, -1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decoder := NewDecoder(testFECSSRC, testMediaSSRC, lookup, logger.GetLogger())
		for j := range media {
			decoder.DecodeFEC(&media[j])
		}
		if recovered := decoder.DecodeFEC(&fecPackets[0]); len(recovered) != 0 {
			b.Fatalf("expected no recovered packets, got %d", len(recovered))
		}
	}
}

func BenchmarkDecoderRecoveryWindow10x1200(b *testing.B) {
	media := benchmarkMediaPackets(10, 1200)
	fecPackets := pionflexfec.NewFlexEncoder03(testFECPT, testFECSSRC).EncodeFec(media, 1)
	lookup := benchmarkMediaLookup(media, 4)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decoder := NewDecoder(testFECSSRC, testMediaSSRC, lookup, logger.GetLogger())
		for j := range media {
			if j != 4 {
				decoder.DecodeFEC(&media[j])
			}
		}
		if recovered := decoder.DecodeFEC(&fecPackets[0]); len(recovered) != 1 {
			b.Fatalf("expected one recovered packet, got %d", len(recovered))
		}
	}
}
