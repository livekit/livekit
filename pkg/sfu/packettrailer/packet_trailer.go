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

import "encoding/binary"

var Magic = [4]byte{'L', 'K', 'T', 'S'}

const (
	xorByte = 0xFF

	envelopeSize = 5 // 1B trailer_len + 4B magic

	tagTimestampUs = 0x01
	tagFrameID     = 0x02
)

type Metadata struct {
	TimestampUs    uint64
	HasTimestampUs bool
	FrameID        uint32
	HasFrameID     bool
}

// StripTrailer returns the number of bytes to strip from the end of an RTP
// payload if it contains an LKTS trailer. The trailer is located by checking
// for the "LKTS" magic suffix and then reading the XORed trailer_len byte
// immediately before it. Returns 0 if absent or ineligible.
func StripTrailer(payload []byte, marker bool) int {
	if !marker || len(payload) < envelopeSize {
		return 0
	}

	tail := payload[len(payload)-4:]
	if tail[0] != Magic[0] || tail[1] != Magic[1] ||
		tail[2] != Magic[2] || tail[3] != Magic[3] {
		return 0
	}

	trailerLen := int(payload[len(payload)-5] ^ xorByte)
	if trailerLen < envelopeSize || trailerLen > len(payload) {
		return 0
	}

	return trailerLen
}

func ParseTrailer(payload []byte, marker bool) (Metadata, bool) {
	strip := StripTrailer(payload, marker)
	if strip == 0 {
		return Metadata{}, false
	}

	var metadata Metadata
	tlvEnd := len(payload) - envelopeSize
	for i := len(payload) - strip; i < tlvEnd; {
		if i+2 > tlvEnd {
			return metadata, true
		}
		tag := payload[i] ^ xorByte
		length := int(payload[i+1] ^ xorByte)
		i += 2
		if length < 0 || i+length > tlvEnd {
			return metadata, true
		}

		switch tag {
		case tagTimestampUs:
			if length == 8 {
				var buf [8]byte
				for j := range buf {
					buf[j] = payload[i+j] ^ xorByte
				}
				metadata.TimestampUs = binary.BigEndian.Uint64(buf[:])
				metadata.HasTimestampUs = true
			}
		case tagFrameID:
			if length == 4 {
				var buf [4]byte
				for j := range buf {
					buf[j] = payload[i+j] ^ xorByte
				}
				metadata.FrameID = binary.BigEndian.Uint32(buf[:])
				metadata.HasFrameID = true
			}
		}
		i += length
	}

	return metadata, true
}
