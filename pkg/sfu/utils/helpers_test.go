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

package utils

import (
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/codecs/mime"
)

func TestFindRTXPayloadType(t *testing.T) {
	rtx := func(pt webrtc.PayloadType, fmtp string) webrtc.RTPCodecParameters {
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime.MimeTypeRTX.String(), ClockRate: 90000, SDPFmtpLine: fmtp},
			PayloadType:        pt,
		}
	}
	vp8 := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP8.String(), ClockRate: 90000},
		PayloadType:        96,
	}

	tests := []struct {
		name     string
		haystack []webrtc.RTPCodecParameters
		expected webrtc.PayloadType
	}{
		{name: "apt only", haystack: []webrtc.RTPCodecParameters{vp8, rtx(97, "apt=96")}, expected: 97},
		{name: "apt with rtx-time", haystack: []webrtc.RTPCodecParameters{vp8, rtx(97, "apt=96;rtx-time=3000")}, expected: 97},
		{name: "rtx-time before apt", haystack: []webrtc.RTPCodecParameters{vp8, rtx(97, "rtx-time=3000; apt=96")}, expected: 97},
		{name: "other codec", haystack: []webrtc.RTPCodecParameters{vp8, rtx(99, "apt=98"), rtx(97, "apt=96")}, expected: 97},
		{name: "apt prefix", haystack: []webrtc.RTPCodecParameters{vp8, rtx(97, "apt=960")}, expected: 0},
		{name: "no rtx", haystack: []webrtc.RTPCodecParameters{vp8}, expected: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, FindRTXPayloadType(96, tc.haystack))
		})
	}
}

func TestIsFECEnabled(t *testing.T) {
	codec := func(mimeType mime.MimeType, fmtp string) webrtc.RTPCodecParameters {
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mimeType.String(), SDPFmtpLine: fmtp},
		}
	}
	opusOn := codec(mime.MimeTypeOpus, "minptime=10;useinbandfec=1")
	opusOff := codec(mime.MimeTypeOpus, "minptime=10;useinbandfec=0")
	red := codec(mime.MimeTypeRED, "111/111")
	vp8 := codec(mime.MimeTypeVP8, "")

	t.Run("fmtp", func(t *testing.T) {
		tests := []struct {
			fmtp     string
			expected bool
		}{
			{"minptime=10;useinbandfec=1", true},
			{"useinbandfec=1", true},
			{"useinbandfec=1;stereo=1", true},
			{"minptime=10; useinbandfec = 1", true},
			{"minptime=10;  useinbandfec=1 ;stereo=1", true},
			{"minptime=10;UseInbandFEC=1", true},
			{"minptime=10;useinbandfec=0", false},
			{"minptime=10;useinbandfec=10", false},
			{"minptime=10;xuseinbandfec=1", false},
			{"minptime=10", false},
			{"", false},
		}
		for _, tc := range tests {
			t.Run(tc.fmtp, func(t *testing.T) {
				opus := codec(mime.MimeTypeOpus, tc.fmtp)
				require.Equal(t, tc.expected, IsFECEnabled(opus, []webrtc.RTPCodecParameters{opus}))
			})
		}
	})

	t.Run("codecs", func(t *testing.T) {
		tests := []struct {
			name     string
			codec    webrtc.RTPCodecParameters
			codecs   []webrtc.RTPCodecParameters
			expected bool
		}{
			{"opus with FEC", opusOn, []webrtc.RTPCodecParameters{opusOn}, true},
			{"opus without FEC", opusOff, []webrtc.RTPCodecParameters{opusOff}, false},
			{"RED over opus with FEC", red, []webrtc.RTPCodecParameters{red, opusOn}, true},
			{"RED over opus without FEC", red, []webrtc.RTPCodecParameters{red, opusOff}, false},
			{"RED without an opus codec", red, []webrtc.RTPCodecParameters{red}, false},
			{"video", vp8, []webrtc.RTPCodecParameters{vp8}, false},
			{"no codec", webrtc.RTPCodecParameters{}, []webrtc.RTPCodecParameters{opusOn}, false},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				require.Equal(t, tc.expected, IsFECEnabled(tc.codec, tc.codecs))
			})
		}
	})
}
