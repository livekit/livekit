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

var Magic = [4]byte{'L', 'K', 'T', 'S'}

const (
	xorByte = 0xFF

	envelopeSize = 5 // 1B trailer_len + 4B magic
)

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
