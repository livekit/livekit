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

package rtptest

import (
	"math/rand"

	"github.com/pion/rtp"
)

// GenerateVP8Packets returns deterministic RTP packets with valid VP8 payload descriptors.
func GenerateVP8Packets(baseSN uint16, count int, payloadType uint8, ssrc uint32) []rtp.Packet {
	rng := rand.New(rand.NewSource(int64(baseSN)))
	packets := make([]rtp.Packet, 0, count)
	for i := 0; i < count; i++ {
		payload := make([]byte, 50+rng.Intn(200))
		rng.Read(payload)
		payload[0] = 0x10

		sequenceNumber := baseSN + uint16(i)
		packets = append(packets, rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    payloadType,
				SequenceNumber: sequenceNumber,
				Timestamp:      90000 + 3000*uint32(sequenceNumber),
				SSRC:           ssrc,
				Marker:         i == count-1,
			},
			Payload: payload,
		})
	}
	return packets
}
