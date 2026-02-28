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

package sfu

import (
	"encoding/binary"
	"testing"
)

func makePayloadWithTrailer(videoLen int, timestampUs int64) []byte {
	payload := make([]byte, videoLen+userTimestampTrailerSize)
	for i := 0; i < videoLen; i++ {
		payload[i] = byte(i)
	}
	binary.BigEndian.PutUint64(payload[videoLen:], uint64(timestampUs))
	copy(payload[videoLen+8:], userTimestampMagic[:])
	return payload
}

func TestStripUserTimestampTrailer(t *testing.T) {
	tests := []struct {
		name      string
		payload   []byte
		marker    bool
		wantStrip int
	}{
		{
			name:      "marker set with valid trailer",
			payload:   makePayloadWithTrailer(20, 1700000000000000),
			marker:    true,
			wantStrip: userTimestampTrailerSize,
		},
		{
			name:      "marker not set with valid trailer",
			payload:   makePayloadWithTrailer(20, 1700000000000000),
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
			name:      "marker set but payload too short",
			payload:   []byte{0x4C, 0x4B, 0x54, 0x53, 0x00, 0x00, 0x00, 0x00},
			marker:    true,
			wantStrip: 0,
		},
		{
			name: "marker set with partial magic mismatch",
			payload: func() []byte {
				p := makePayloadWithTrailer(20, 1700000000000000)
				p[len(p)-1] = 'x' // corrupt 'S' -> 'x'
				return p
			}(),
			marker:    true,
			wantStrip: 0,
		},
		{
			name:      "exactly trailer size with valid magic",
			payload:   makePayloadWithTrailer(0, 42),
			marker:    true,
			wantStrip: userTimestampTrailerSize,
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
			got := stripUserTimestampTrailer(tt.payload, tt.marker)
			if got != tt.wantStrip {
				t.Errorf("stripUserTimestampTrailer() = %d, want %d", got, tt.wantStrip)
			}
		})
	}
}
