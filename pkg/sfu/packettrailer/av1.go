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

// AV1 frames carry the trailer in a metadata OBU (obu_type 5) of
// metadata_type 31 instead of after the frame, placed after the temporal
// delimiter and sequence header; its payload is the LKTS trailer. This
// matches the client SDKs (packet_trailer_av1.cpp).
const (
	av1ObuSizePresent   = 0x02
	av1ObuExtension     = 0x04
	av1ObuTypeSeqHeader = 1
	av1ObuTypeTD        = 2
	av1ObuTypeMetadata  = 5

	av1MetadataTypeLiveKitTrailer = 31
)

type av1Obu struct {
	typ          byte
	start, end   int // the whole OBU
	payloadStart int
}

// nextAV1Obu parses the OBU at pos of a low-overhead bitstream.
func nextAV1Obu(b []byte, pos int) (av1Obu, bool) {
	o := av1Obu{start: pos}
	if pos >= len(b) || b[pos]&0x80 != 0 {
		return o, false
	}
	hdr := b[pos]
	o.typ = (hdr >> 3) & 0x0f
	pos++
	if hdr&av1ObuExtension != 0 {
		if pos >= len(b) {
			return o, false
		}
		pos++
	}
	size := uint64(len(b) - pos)
	if hdr&av1ObuSizePresent != 0 {
		v, n := readLeb128(b[pos:])
		if n == 0 || v > uint64(len(b)-pos-n) {
			return o, false
		}
		size, pos = v, pos+n
	}
	o.payloadStart, o.end = pos, pos+int(size)
	return o, true
}

// ExtractAV1 finds the trailer metadata OBU in an AV1 temporal unit and
// returns its metadata and the temporal unit without it.
func ExtractAV1(tu []byte) (Metadata, []byte, bool) {
	for pos := 0; pos < len(tu); {
		o, ok := nextAV1Obu(tu, pos)
		if !ok {
			return Metadata{}, tu, false
		}
		if o.typ == av1ObuTypeMetadata {
			payload := tu[o.payloadStart:o.end]
			if mt, n := readLeb128(payload); n > 0 && mt == av1MetadataTypeLiveKitTrailer {
				trailer := payload[n:]
				if m, tn := ParseTrailer(trailer, true); tn == len(trailer) && tn > 0 {
					out := make([]byte, 0, len(tu)-(o.end-o.start))
					out = append(out, tu[:o.start]...)
					return m, append(out, tu[o.end:]...), true
				}
			}
		}
		pos = o.end
	}
	return Metadata{}, tu, false
}

// InsertAV1 adds a trailer metadata OBU carrying m to an AV1 temporal unit,
// after its temporal delimiter and sequence header.
func InsertAV1(tu []byte, m Metadata) []byte {
	trailer := AppendTrailer(nil, m)
	if len(trailer) == 0 {
		return tu
	}
	payload := appendLeb128(nil, av1MetadataTypeLiveKitTrailer)
	payload = append(payload, trailer...)
	obu := []byte{av1ObuTypeMetadata<<3 | av1ObuSizePresent}
	obu = appendLeb128(obu, uint64(len(payload)))
	obu = append(obu, payload...)

	offset := 0
	for pos := 0; pos < len(tu); {
		o, ok := nextAV1Obu(tu, pos)
		if !ok {
			offset = 0
			break
		}
		if o.typ == av1ObuTypeTD {
			pos = o.end
			continue
		}
		if o.typ != av1ObuTypeSeqHeader || tu[o.start]&av1ObuSizePresent == 0 {
			// an OBU without a size field runs to the end of the temporal
			// unit: the trailer goes before it, not into it
			break
		}
		offset = o.end
		pos = o.end
	}
	out := make([]byte, 0, len(tu)+len(obu))
	out = append(out, tu[:offset]...)
	out = append(out, obu...)
	return append(out, tu[offset:]...)
}

func readLeb128(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 8; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

func appendLeb128(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}
