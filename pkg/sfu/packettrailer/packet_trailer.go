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

	if !ShrinkLastAggregatedNALU(payload, mimeType, trailerLen) {
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
