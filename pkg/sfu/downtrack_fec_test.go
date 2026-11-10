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

package sfu

import (
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/codecs/mime"
)

func TestIsUpstreamFECEnabled(t *testing.T) {
	codec := func(mimeType mime.MimeType, fmtp string) webrtc.RTPCodecParameters {
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mimeType.String(), SDPFmtpLine: fmtp},
		}
	}
	opusOn := codec(mime.MimeTypeOpus, "minptime=10;useinbandfec=1")
	opusOff := codec(mime.MimeTypeOpus, "minptime=10;useinbandfec=0")
	red := codec(mime.MimeTypeRED, "111/111")
	vp8 := codec(mime.MimeTypeVP8, "")

	tests := []struct {
		name     string
		matched  webrtc.RTPCodecParameters
		upstream []webrtc.RTPCodecParameters
		expected bool
	}{
		{"opus with FEC", opusOn, []webrtc.RTPCodecParameters{opusOn}, true},
		{"opus without FEC", opusOff, []webrtc.RTPCodecParameters{opusOff}, false},
		{"RED over opus with FEC", red, []webrtc.RTPCodecParameters{red, opusOn}, true},
		{"RED over opus without FEC", red, []webrtc.RTPCodecParameters{red, opusOff}, false},
		{"RED without an upstream opus codec", red, []webrtc.RTPCodecParameters{red}, false},
		{"video", vp8, []webrtc.RTPCodecParameters{vp8}, false},
		{"no matched codec", webrtc.RTPCodecParameters{}, []webrtc.RTPCodecParameters{opusOn}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, isUpstreamFECEnabled(tc.matched, tc.upstream))
		})
	}
}
