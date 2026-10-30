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
	"strconv"
	"strings"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/codecs/mime"
)

var Magic = [4]byte{'L', 'K', 'T', 'S'}

const (
	xorByte = 0xFF

	envelopeSize = 5 // 1B trailer_len + 4B magic
)

// StripTrailer returns the number of bytes to strip from the end of an RTP
// payload if it contains an LKTS trailer. The trailer is located by checking
// for the "LKTS" magic suffix and then reading the XORed trailer_len byte
// immediately before it. isEndOfFrame must be set only for packets ending an
// encoded frame, i. e. where a trailer could have been appended. Returns 0 if
// absent or ineligible.
func StripTrailer(payload []byte, isEndOfFrame bool) int {
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

// Strip removes n trailer bytes, as returned by StripTrailer, from the end of
// payload. The trailer is part of the frame's last NALU, so in an H.264 STAP-A
// or H.265 aggregation packet the size of the NALU ending the payload shrinks
// by n too.
func Strip(payload []byte, n int, codec webrtc.RTPCodecCapability) []byte {
	switch mime.NormalizeMimeType(codec.MimeType) {
	case mime.MimeTypeH264:
		if len(payload) > 0 && payload[0]&0x1f == 24 { // STAP-A
			shrinkLastNALU(payload, 1, 0, n)
		}
	case mime.MimeTypeH265:
		if len(payload) > 1 && (payload[0]>>1)&0x3f == 48 { // AP
			if hasDONL(codec.SDPFmtpLine) {
				// a 2-byte DONL before the first NALU, a 1-byte DOND before the others
				shrinkLastNALU(payload, 4, 1, n)
			} else {
				shrinkLastNALU(payload, 2, 0, n)
			}
		}
	}
	return payload[:len(payload)-n]
}

// hasDONL reports whether H.265 aggregation units carry decoding order
// numbers, which they do when sprop-max-don-diff is above 0 (RFC 7798 7.1).
func hasDONL(fmtp string) bool {
	for _, param := range strings.Split(fmtp, ";") {
		key, value, _ := strings.Cut(strings.TrimSpace(param), "=")
		if strings.EqualFold(key, "sprop-max-don-diff") {
			diff, err := strconv.Atoi(value)
			return err == nil && diff > 0
		}
	}
	return false
}

// shrinkLastNALU walks the size prefixed NALUs of an aggregation packet, from
// pos, skipping skip bytes between them, and shrinks the one ending the
// payload by n.
func shrinkLastNALU(payload []byte, pos int, skip int, n int) {
	for pos+2 <= len(payload) {
		size := int(payload[pos])<<8 | int(payload[pos+1])
		end := pos + 2 + size
		if end == len(payload) && size > n {
			size -= n
			payload[pos], payload[pos+1] = byte(size>>8), byte(size)
		}
		if end >= len(payload) {
			return
		}
		pos = end + skip
	}
}
