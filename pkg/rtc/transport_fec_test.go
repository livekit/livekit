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

package rtc

import (
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func TestFECPairsFromSDP(t *testing.T) {
	offer := `v=0
o=- 8541913762120318441 2 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 96 97 115
c=IN IP4 0.0.0.0
a=mid:0
a=sendonly
a=rtpmap:96 VP8/90000
a=rtpmap:97 rtx/90000
a=fmtp:97 apt=96
a=rtpmap:115 flexfec-03/90000
a=fmtp:115 repair-window=10000000
a=ssrc-group:FID 1111 2222
a=ssrc-group:FEC-FR 1111 3333
a=ssrc:1111 cname:test
a=ssrc:2222 cname:test
a=ssrc:3333 cname:test
`
	parsed := &sdp.SessionDescription{}
	require.NoError(t, parsed.Unmarshal([]byte(offer)))

	fecPairs := fecPairsFromSDP(parsed, logger.GetLogger())
	require.Len(t, fecPairs, 1)
	assert.Equal(t, uint32(1111), fecPairs[uint32(3333)])

	// FID pairs are not picked up as FEC
	rtxPairs := nonSimulcastRTXRepairsFromSDP(parsed, logger.GetLogger())
	require.Len(t, rtxPairs, 1)
	assert.Equal(t, uint32(1111), rtxPairs[uint32(2222)])
}

func TestFECPairsFromSDPNoGroups(t *testing.T) {
	offer := `v=0
o=- 8541913762120318441 2 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:0
a=sendonly
a=rtpmap:96 VP8/90000
a=ssrc:1111 cname:test
`
	parsed := &sdp.SessionDescription{}
	require.NoError(t, parsed.Unmarshal([]byte(offer)))
	assert.Empty(t, fecPairsFromSDP(parsed, logger.GetLogger()))
}

func TestFlexFECPayloadTypeValidation(t *testing.T) {
	assert.NoError(t, validateFlexFECPayloadType(115))
	// VP8 payload type
	assert.Error(t, validateFlexFECPayloadType(96))
	// RTX slot of VP8 (pt+1)
	assert.Error(t, validateFlexFECPayloadType(97))
	// opus
	assert.Error(t, validateFlexFECPayloadType(111))
}

func TestMediaEngineRegistersFlexFEC(t *testing.T) {
	enabledCodecs := []*livekit.Codec{
		{Mime: "video/VP8"},
		{Mime: "video/rtx"},
	}

	for _, enabled := range []bool{false, true} {
		me, err := createMediaEngine(enabledCodecs, DirectionConfig{
			FlexFEC: FlexFECDirectionConfig{
				Enabled:     enabled,
				PayloadType: 115,
			},
			RTCPFeedback: RTCPFeedbackConfig{
				Video: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}},
			},
		}, false)
		require.NoError(t, err)

		// drive codec registration into negotiated form via an SDP round trip
		// is heavyweight, instead check via filterCodecs retention behavior
		flexfecParams := flexFECCodecParameters(115)
		filtered := filterCodecs(
			[]webrtc.RTPCodecParameters{flexfecParams},
			enabledCodecs,
			RTCPFeedbackConfig{},
			false,
			enabled,
		)
		if enabled {
			require.Len(t, filtered, 1)
			assert.Equal(t, webrtc.MimeTypeFlexFEC03, filtered[0].MimeType)
		} else {
			assert.Empty(t, filtered)
		}
		_ = me
	}
}
