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

	"github.com/livekit/livekit-server/pkg/config"
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

func TestFECPairsFromSDPIgnoresMalformedGroups(t *testing.T) {
	description := &sdp.SessionDescription{
		MediaDescriptions: []*sdp.MediaDescription{{
			Attributes: []sdp.Attribute{
				{Key: sdp.AttrKeySSRCGroup},
				{Key: sdp.AttrKeySSRCGroup, Value: "FEC-FR 1111"},
				{Key: sdp.AttrKeySSRCGroup, Value: "FEC-FR invalid 3333"},
				{Key: sdp.AttrKeySSRCGroup, Value: "FEC-FR 1111 invalid"},
				{Key: sdp.AttrKeySSRCGroup, Value: "FEC-FR 1111 3333 4444"},
			},
		}},
	}

	require.NotPanics(t, func() {
		assert.Empty(t, fecPairsFromSDP(description, logger.GetLogger()))
	})
}

func TestFECPairsFromSDPHandlesWhitespace(t *testing.T) {
	description := &sdp.SessionDescription{
		MediaDescriptions: []*sdp.MediaDescription{{
			Attributes: []sdp.Attribute{{
				Key:   sdp.AttrKeySSRCGroup,
				Value: "  FEC-FR   1111\t3333  ",
			}},
		}},
	}

	assert.Equal(t, map[uint32]uint32{3333: 1111}, fecPairsFromSDP(description, logger.GetLogger()))
}

func TestFlexFECPayloadTypeValidation(t *testing.T) {
	assert.NoError(t, validateFlexFECPayloadType(115))
	// upper boundary of the 7-bit RTP payload type field
	assert.NoError(t, validateFlexFECPayloadType(127))
	assert.Error(t, validateFlexFECPayloadType(128))
	assert.Error(t, validateFlexFECPayloadType(255))
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

	for _, test := range []struct {
		name    string
		enabled bool
	}{
		{name: "disabled", enabled: false},
		{name: "enabled", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			me, err := createMediaEngine(enabledCodecs, DirectionConfig{
				FlexFEC: FlexFECDirectionConfig{
					Enabled:     test.enabled,
					PayloadType: 115,
				},
				RTCPFeedback: RTCPFeedbackConfig{
					Video: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}},
				},
			}, false)
			require.NoError(t, err)

			pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
			require.NoError(t, err)
			defer pc.Close()
			_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
			require.NoError(t, err)
			offer, err := pc.CreateOffer(nil)
			require.NoError(t, err)

			flexFECParams := flexFECCodecParameters(115)
			assert.Equal(t, "repair-window=10000000", flexFECParams.SDPFmtpLine)
			filtered := filterCodecs(
				[]webrtc.RTPCodecParameters{flexFECParams},
				enabledCodecs,
				RTCPFeedbackConfig{},
				false,
				test.enabled,
			)
			if test.enabled {
				require.Len(t, filtered, 1)
				assert.Equal(t, webrtc.MimeTypeFlexFEC03, filtered[0].MimeType)
				assert.Contains(t, offer.SDP, "a=rtpmap:115 flexfec-03/90000")
				assert.Contains(t, offer.SDP, "a=fmtp:115 repair-window=10000000")
			} else {
				assert.Empty(t, filtered)
				assert.NotContains(t, offer.SDP, "flexfec-03")
			}
		})
	}
}

func TestWebRTCConfigFlexFEC(t *testing.T) {
	newConfig := func(t *testing.T) *config.Config {
		t.Helper()
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.RTC.TCPPort = 0
		return conf
	}

	t.Run("defaults and publisher updates", func(t *testing.T) {
		conf := newConfig(t)
		conf.RTC.FlexFEC = config.FlexFECConfig{UpstreamEnabled: true}

		webRTCConfig, err := NewWebRTCConfig(conf)
		require.NoError(t, err)
		assert.Equal(t, FlexFECDirectionConfig{
			Enabled:     true,
			PayloadType: config.DefaultFlexFECConfig.PayloadType,
		}, webRTCConfig.Publisher.FlexFEC)
		assert.False(t, webRTCConfig.Subscriber.FlexFEC.Enabled)

		webRTCConfig.UpdatePublisherConfig(true)
		assert.True(t, webRTCConfig.Publisher.FlexFEC.Enabled)
		assert.Equal(t, config.DefaultFlexFECConfig.PayloadType, webRTCConfig.Publisher.FlexFEC.PayloadType)
	})

	t.Run("invalid payload type", func(t *testing.T) {
		conf := newConfig(t)
		conf.RTC.FlexFEC = config.FlexFECConfig{
			UpstreamEnabled: true,
			PayloadType:     96,
		}

		_, err := NewWebRTCConfig(conf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "collides")
	})
}
