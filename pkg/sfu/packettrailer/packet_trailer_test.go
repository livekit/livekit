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

package packettrailer

import (
	"encoding/binary"
	"testing"
)

const (
	tagTimestampUs = 0x01
	tagFrameID     = 0x02
)

// appendTLV appends a single XORed TLV element to dst.
func appendTLV(dst []byte, tag byte, value []byte) []byte {
	dst = append(dst, tag^xorByte, byte(len(value))^xorByte)
	for _, b := range value {
		dst = append(dst, b^xorByte)
	}
	return dst
}

// appendEnvelope appends the 5-byte envelope (XORed trailer_len + magic).
func appendEnvelope(dst []byte, trailerLen byte) []byte {
	dst = append(dst, trailerLen^xorByte)
	dst = append(dst, Magic[:]...)
	return dst
}

// makeTrailer builds a complete LKTS trailer with both timestamp and frame_id TLVs.
func makeTrailer(timestampUs int64, frameID uint32) []byte {
	var trailer []byte

	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(timestampUs))
	trailer = appendTLV(trailer, tagTimestampUs, tsBuf[:])

	var fidBuf [4]byte
	binary.BigEndian.PutUint32(fidBuf[:], frameID)
	trailer = appendTLV(trailer, tagFrameID, fidBuf[:])

	trailerLen := byte(len(trailer) + envelopeSize)
	trailer = appendEnvelope(trailer, trailerLen)
	return trailer
}

// makePayloadWithTrailer builds a fake video payload followed by a full LKTS trailer.
func makePayloadWithTrailer(videoLen int, timestampUs int64, frameID uint32) []byte {
	video := make([]byte, videoLen)
	for i := range video {
		video[i] = byte(i)
	}
	return append(video, makeTrailer(timestampUs, frameID)...)
}

// makeTimestampOnlyTrailer builds a trailer with only the timestamp TLV.
func makeTimestampOnlyTrailer(timestampUs int64) []byte {
	var trailer []byte
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(timestampUs))
	trailer = appendTLV(trailer, tagTimestampUs, tsBuf[:])
	trailerLen := byte(len(trailer) + envelopeSize)
	trailer = appendEnvelope(trailer, trailerLen)
	return trailer
}

func TestStripTrailer(t *testing.T) {
	fullTrailerSize := 21 // (1+1+8) + (1+1+4) + 5
	tsOnlyTrailerSize := 15 // (1+1+8) + 5

	tests := []struct {
		name      string
		payload   []byte
		marker    bool
		wantStrip int
	}{
		{
			name:      "marker set with full trailer (timestamp + frame_id)",
			payload:   makePayloadWithTrailer(20, 1700000000000000, 42),
			marker:    true,
			wantStrip: fullTrailerSize,
		},
		{
			name: "marker set with timestamp-only trailer",
			payload: func() []byte {
				video := make([]byte, 20)
				return append(video, makeTimestampOnlyTrailer(1700000000000000)...)
			}(),
			marker:    true,
			wantStrip: tsOnlyTrailerSize,
		},
		{
			name:      "marker not set with valid trailer",
			payload:   makePayloadWithTrailer(20, 1700000000000000, 42),
			marker:    false,
			wantStrip: 0,
		},
		{
			name:      "marker set without magic",
			payload:   make([]byte, 32),
			marker:    true,
			wantStrip: 0,
		},
		{
			name:      "marker set but payload too short for envelope",
			payload:   []byte{0x4C, 0x4B, 0x54, 0x53},
			marker:    true,
			wantStrip: 0,
		},
		{
			name: "marker set with partial magic mismatch",
			payload: func() []byte {
				p := makePayloadWithTrailer(20, 1700000000000000, 42)
				p[len(p)-1] = 'x'
				return p
			}(),
			marker:    true,
			wantStrip: 0,
		},
		{
			name: "trailer_len exceeds payload length",
			payload: func() []byte {
				var trailer []byte
				var tsBuf [8]byte
				binary.BigEndian.PutUint64(tsBuf[:], 42)
				trailer = appendTLV(trailer, tagTimestampUs, tsBuf[:])
				trailer = appendEnvelope(trailer, 200)
				return trailer
			}(),
			marker:    true,
			wantStrip: 0,
		},
		{
			name: "trailer_len smaller than envelope (invalid)",
			payload: func() []byte {
				video := make([]byte, 20)
				var trailer []byte
				var tsBuf [8]byte
				binary.BigEndian.PutUint64(tsBuf[:], 42)
				trailer = appendTLV(trailer, tagTimestampUs, tsBuf[:])
				trailer = appendEnvelope(trailer, 3)
				return append(video, trailer...)
			}(),
			marker:    true,
			wantStrip: 0,
		},
		{
			name:      "exactly envelope-only trailer",
			payload:   appendEnvelope(nil, byte(envelopeSize)),
			marker:    true,
			wantStrip: envelopeSize,
		},
		{
			name:      "empty payload",
			payload:   []byte{},
			marker:    true,
			wantStrip: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripTrailer(tt.payload, tt.marker)
			if got != tt.wantStrip {
				t.Errorf("StripTrailer() = %d, want %d", got, tt.wantStrip)
			}
		})
	}
}
