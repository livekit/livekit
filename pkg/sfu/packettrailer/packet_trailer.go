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

	"github.com/livekit/protocol/codecs/mime"
)

var Magic = [4]byte{'L', 'K', 'T', 'S'}

const (
	xorByte = 0xFF

	envelopeSize = 5 // 1B trailer_len + 4B magic

	h264STAPA = 0x18
	h265AP    = 0x30
)

// StripTrailer returns the number of bytes to strip from the end of an RTP
// payload if it contains an LKTS trailer. The trailer is located by checking
// for the "LKTS" magic suffix and then reading the XORed trailer_len byte
// immediately before it. isEndOfFrame must be set only for packets ending an
// encoded frame, i. e. where a trailer could have been appended. Returns 0 if
// absent or ineligible.
//
// The trailer is appended to the encoded frame before packetization, so in an
// aggregation packet it is counted in the size of the last NAL unit. That size
// is reduced in place, see ShrinkLastAggregatedNALU.
func StripTrailer(payload []byte, mimeType mime.MimeType, isEndOfFrame bool) int {
	trailerLen := trailerLength(payload, isEndOfFrame)
	if trailerLen == 0 || !ShrinkLastAggregatedNALU(payload, mimeType, trailerLen) {
		return 0
	}
	return trailerLen
}

func trailerLength(payload []byte, isEndOfFrame bool) int {
	if !isEndOfFrame || len(payload) < envelopeSize {
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

// ShrinkLastAggregatedNALU reduces, in place, the size field of the last NAL
// unit in an H.264 STAP-A or H.265 aggregation packet by n bytes, so that the
// packet stays well formed once n bytes are cut from its end. Other payloads
// are left untouched. Returns false if the payload is an aggregation packet
// whose last NAL unit does not end the payload or is not longer than n.
func ShrinkLastAggregatedNALU(payload []byte, mimeType mime.MimeType, n int) bool {
	var offset int
	switch {
	case mimeType == mime.MimeTypeH264 && len(payload) >= 1 && payload[0]&0x1F == h264STAPA:
		offset = 1
	case mimeType == mime.MimeTypeH265 && len(payload) >= 2 && (payload[0]>>1)&0x3F == h265AP:
		offset = 2
	default:
		return true
	}

	for offset+2 <= len(payload) {
		size := int(binary.BigEndian.Uint16(payload[offset:]))
		end := offset + 2 + size
		if end == len(payload) {
			if size <= n {
				return false
			}
			binary.BigEndian.PutUint16(payload[offset:], uint16(size-n))
			return true
		}
		offset = end
	}
	return false
}

const (
	trailerTagTimestampUs = 0x01
	trailerTagFrameID     = 0x02
	trailerTagUserData    = 0x03
)

// Metadata is the content of an LKTS trailer.
type Metadata struct {
	UserTimestampUs    int64
	HasUserTimestampUs bool
	FrameID            uint32
	HasFrameID         bool
	UserData           []byte
}

func (m Metadata) IsEmpty() bool {
	return !m.HasUserTimestampUs && !m.HasFrameID && len(m.UserData) == 0
}

// ParseTrailer decodes an LKTS trailer and returns it with the number of bytes
// it occupies at the end of the payload (0 if absent).
func ParseTrailer(payload []byte, isEndOfFrame bool) (Metadata, int) {
	var metadata Metadata
	trailerLen := trailerLength(payload, isEndOfFrame)
	if trailerLen == 0 {
		return metadata, 0
	}

	tlv := payload[len(payload)-trailerLen : len(payload)-envelopeSize]
	for len(tlv) != 0 {
		if len(tlv) < 2 {
			return metadata, trailerLen
		}
		tag := tlv[0] ^ xorByte
		valueLen := int(tlv[1] ^ xorByte)
		tlv = tlv[2:]
		if valueLen > len(tlv) {
			return metadata, trailerLen
		}
		value := tlv[:valueLen]
		tlv = tlv[valueLen:]

		switch tag {
		case trailerTagTimestampUs:
			if valueLen == 8 {
				metadata.UserTimestampUs = int64(xorUint64(value))
				metadata.HasUserTimestampUs = true
			}
		case trailerTagFrameID:
			if valueLen == 4 {
				metadata.FrameID = uint32(xorUint64(value))
				metadata.HasFrameID = true
			}
		case trailerTagUserData:
			metadata.UserData = make([]byte, valueLen)
			for i, b := range value {
				metadata.UserData[i] = b ^ xorByte
			}
		}
	}

	return metadata, trailerLen
}

// AppendTrailer appends an LKTS trailer carrying m to payload. Like the
// client SDKs, it drops user data that does not fit the 255 byte trailer
// next to the timestamp and frame id, and returns payload unchanged when
// nothing is left to embed.
func AppendTrailer(payload []byte, m Metadata) []byte {
	if m.IsEmpty() {
		return payload
	}
	var tlv []byte
	if m.HasUserTimestampUs {
		tlv = append(tlv, trailerTagTimestampUs^xorByte, 8^xorByte)
		for i := 7; i >= 0; i-- {
			tlv = append(tlv, byte(uint64(m.UserTimestampUs)>>(8*i))^xorByte)
		}
	}
	if m.HasFrameID {
		tlv = append(tlv, trailerTagFrameID^xorByte, 4^xorByte)
		for i := 3; i >= 0; i-- {
			tlv = append(tlv, byte(m.FrameID>>(8*i))^xorByte)
		}
	}
	if budget := 255 - len(tlv) - envelopeSize - 2; len(m.UserData) > 0 && len(m.UserData) <= budget {
		tlv = append(tlv, trailerTagUserData^xorByte, byte(len(m.UserData))^xorByte)
		for _, b := range m.UserData {
			tlv = append(tlv, b^xorByte)
		}
	}
	if len(tlv) == 0 {
		return payload
	}
	total := len(tlv) + envelopeSize
	payload = append(payload, tlv...)
	payload = append(payload, byte(total)^xorByte)
	return append(payload, Magic[:]...)
}

func xorUint64(value []byte) uint64 {
	var out uint64
	for _, b := range value {
		out = (out << 8) | uint64(b^xorByte)
	}
	return out
}
