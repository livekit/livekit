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

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
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

// aggregate builds an aggregation packet payload: header, then each NALU
// prefixed by its 2-byte size and, after the first, by sep.
func aggregate(header []byte, sep []byte, nalus ...[]byte) []byte {
	p := append([]byte{}, header...)
	for i, nalu := range nalus {
		if i > 0 {
			p = append(p, sep...)
		}
		p = binary.BigEndian.AppendUint16(p, uint16(len(nalu)))
		p = append(p, nalu...)
	}
	return p
}

func TestStrip(t *testing.T) {
	trailer := makeTrailer(1700000000000000, 42)
	withTrailer := func(slice []byte) []byte { return append(append([]byte{}, slice...), trailer...) }
	sps := []byte{0x67, 0x42, 0xc0, 0x28}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	slice := []byte{0x65, 0x88, 0x80, 0x40}
	vps265 := []byte{0x40, 0x01, 0x0c, 0x01}
	sps265 := []byte{0x42, 0x01, 0x01, 0x01}
	pps265 := []byte{0x44, 0x01, 0xc1, 0x72}
	slice265 := []byte{0x28, 0x01, 0xaf, 0x71}
	h264STAPA, h265AP := []byte{0x78}, []byte{0x60, 0x01}
	h265APDONL := []byte{0x60, 0x01, 0x12, 0x34} // AP header and DONL
	dond := []byte{0x00}

	h264 := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}
	h265 := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265}
	h265DONL := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265, SDPFmtpLine: "profile-id=1; sprop-max-don-diff=2"}
	vp8 := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}

	tests := []struct {
		name    string
		payload []byte
		codec   webrtc.RTPCodecCapability
		want    []byte
	}{
		{
			name:    "H.264 single NALU",
			payload: withTrailer(slice),
			codec:   h264,
			want:    slice,
		},
		{
			name:    "H.264 STAP-A",
			payload: aggregate(h264STAPA, nil, sps, pps, withTrailer(slice)),
			codec:   h264,
			want:    aggregate(h264STAPA, nil, sps, pps, slice),
		},
		{
			name:    "H.265 AP",
			payload: aggregate(h265AP, nil, vps265, sps265, pps265, withTrailer(slice265)),
			codec:   h265,
			want:    aggregate(h265AP, nil, vps265, sps265, pps265, slice265),
		},
		{
			name:    "H.265 AP with DONL",
			payload: aggregate(h265APDONL, dond, vps265, sps265, pps265, withTrailer(slice265)),
			codec:   h265DONL,
			want:    aggregate(h265APDONL, dond, vps265, sps265, pps265, slice265),
		},
		{
			name:    "VP8",
			payload: append([]byte{0x10}, withTrailer(slice)...),
			codec:   vp8,
			want:    append([]byte{0x10}, slice...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := append([]byte{}, tt.payload...)
			n := StripTrailer(payload, true)
			got := Strip(payload, n, tt.codec)
			if string(got) != string(tt.want) {
				t.Errorf("Strip() = %x, want %x", got, tt.want)
			}

			// what a subscriber's depacketizer makes of it
			switch tt.codec.MimeType {
			case webrtc.MimeTypeH264:
				if _, err := (&codecs.H264Packet{}).Unmarshal(got); err != nil {
					t.Errorf("H.264 depacketizer rejected %x: %v", got, err)
				}
			case webrtc.MimeTypeH265:
				p := &codecs.H265Packet{}
				p.WithDONL(hasDONL(tt.codec.SDPFmtpLine))
				if _, err := p.Unmarshal(got); err != nil {
					t.Errorf("H.265 depacketizer rejected %x: %v", got, err)
				}
			}
		})
	}
}

func TestHasDONL(t *testing.T) {
	for fmtp, want := range map[string]bool{
		"":                                   false,
		"profile-id=1":                       false,
		"sprop-max-don-diff=0":               false,
		"profile-id=1;sprop-max-don-diff=3":  true,
		"profile-id=1; SPROP-MAX-DON-DIFF=1": true,
	} {
		if got := hasDONL(fmtp); got != want {
			t.Errorf("hasDONL(%q) = %v, want %v", fmtp, got, want)
		}
	}
}
