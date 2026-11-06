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
