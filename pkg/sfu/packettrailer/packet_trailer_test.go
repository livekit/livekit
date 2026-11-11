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
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/pion/rtp/codecs"

	"github.com/livekit/protocol/codecs/mime"
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
	fullTrailerSize := 21   // (1+1+8) + (1+1+4) + 5
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
			got := StripTrailer(tt.payload, mime.MimeTypeVP8, tt.marker)
			if got != tt.wantStrip {
				t.Errorf("StripTrailer() = %d, want %d", got, tt.wantStrip)
			}
		})
	}
}

// makeAggregate builds an aggregation packet with the given payload header and
// NAL units, appending trailer to the last NAL unit and counting it in its size.
func makeAggregate(header []byte, nalus [][]byte, trailer []byte) []byte {
	p := append([]byte{}, header...)
	for i, nalu := range nalus {
		size := len(nalu)
		if i == len(nalus)-1 {
			size += len(trailer)
		}
		p = binary.BigEndian.AppendUint16(p, uint16(size))
		p = append(p, nalu...)
	}
	return append(p, trailer...)
}

func TestStripTrailerAggregationPacket(t *testing.T) {
	h264NALUs := [][]byte{
		{0x67, 0x42, 0xC0, 0x1F},       // SPS
		{0x68, 0xCE, 0x3C, 0x80},       // PPS
		{0x65, 0x88, 0x84, 0x00, 0x33}, // IDR slice
	}
	h265NALUs := [][]byte{
		{0x40, 0x01, 0x0C, 0x01},       // VPS
		{0x42, 0x01, 0x01, 0x01},       // SPS
		{0x44, 0x01, 0xC1, 0x72},       // PPS
		{0x26, 0x01, 0xAF, 0x08, 0x40}, // IDR slice
	}
	trailer := makeTrailer(1700000000000000, 42)

	unmarshalH264 := func(p []byte) ([]byte, error) {
		return (&codecs.H264Packet{}).Unmarshal(p)
	}
	unmarshalH265 := func(p []byte) ([]byte, error) {
		d := &codecs.H265Depacketizer{}
		return d.Unmarshal(p)
	}

	tests := []struct {
		name      string
		mimeType  mime.MimeType
		payload   []byte
		want      []byte
		unmarshal func([]byte) ([]byte, error)
	}{
		{
			name:      "H.264 STAP-A",
			mimeType:  mime.MimeTypeH264,
			payload:   makeAggregate([]byte{0x78}, h264NALUs, trailer),
			want:      makeAggregate([]byte{0x78}, h264NALUs, nil),
			unmarshal: unmarshalH264,
		},
		{
			name:      "H.264 STAP-A with a single NAL unit",
			mimeType:  mime.MimeTypeH264,
			payload:   makeAggregate([]byte{0x78}, h264NALUs[2:], trailer),
			want:      makeAggregate([]byte{0x78}, h264NALUs[2:], nil),
			unmarshal: unmarshalH264,
		},
		{
			name:      "H.265 aggregation packet",
			mimeType:  mime.MimeTypeH265,
			payload:   makeAggregate([]byte{0x60, 0x01}, h265NALUs, trailer),
			want:      makeAggregate([]byte{0x60, 0x01}, h265NALUs, nil),
			unmarshal: unmarshalH265,
		},
		{
			name:      "H.264 single NAL unit",
			mimeType:  mime.MimeTypeH264,
			payload:   append(append([]byte{}, h264NALUs[2]...), trailer...),
			want:      h264NALUs[2],
			unmarshal: unmarshalH264,
		},
		{
			name:      "H.264 FU-A end fragment",
			mimeType:  mime.MimeTypeH264,
			payload:   append([]byte{0x7C, 0x45, 0x01, 0x02, 0x03}, trailer...),
			want:      []byte{0x7C, 0x45, 0x01, 0x02, 0x03},
			unmarshal: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := StripTrailer(tt.payload, tt.mimeType, true)
			if n != len(trailer) {
				t.Fatalf("StripTrailer() = %d, want %d", n, len(trailer))
			}
			got := tt.payload[:len(tt.payload)-n]
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("stripped payload = %x, want %x", got, tt.want)
			}
			if tt.unmarshal != nil {
				if _, err := tt.unmarshal(got); err != nil {
					t.Fatalf("depacketizer rejected stripped payload: %v", err)
				}
			}
		})
	}
}

func TestStripTrailerMalformedAggregationPacket(t *testing.T) {
	trailer := makeTrailer(1700000000000000, 42)
	nalus := [][]byte{{0x67, 0x42, 0xC0, 0x1F}, {0x65, 0x88, 0x84}}

	tests := []struct {
		name    string
		payload []byte
	}{
		{
			// trailer not counted in the last NAL unit, e.g. split off by a start code inside it
			name:    "last NAL unit does not end the payload",
			payload: append(makeAggregate([]byte{0x78}, nalus, nil), trailer...),
		},
		{
			name:    "trailer is the whole last NAL unit",
			payload: makeAggregate([]byte{0x78}, [][]byte{nalus[0], {}}, trailer),
		},
		{
			name:    "truncated size field",
			payload: append([]byte{0x78, 0x00}, trailer...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := append([]byte{}, tt.payload...)
			if n := StripTrailer(tt.payload, mime.MimeTypeH264, true); n != 0 {
				t.Fatalf("StripTrailer() = %d, want 0", n)
			}
			if !bytes.Equal(tt.payload, orig) {
				t.Fatalf("payload modified: %x, want %x", tt.payload, orig)
			}
		})
	}
}
