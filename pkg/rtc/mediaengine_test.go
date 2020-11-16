package rtc

import (
	"testing"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
)

const sdpValue = `v=0
o=- 884433216 1576829404 IN IP4 0.0.0.0
s=-
t=0 0
a=fingerprint:sha-256 1D:6B:6D:18:95:41:F9:BC:E4:AC:25:6A:26:A3:C8:09:D2:8C:EE:1B:7D:54:53:33:F7:E3:2C:0D:FE:7A:9D:6B
a=group:BUNDLE 0 1 2
m=audio 9 UDP/TLS/RTP/SAVPF 0 8 111 9
c=IN IP4 0.0.0.0
a=mid:0
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:111 opus/48000/2
a=fmtp:111 minptime=10;useinbandfec=1
a=rtpmap:9 G722/8000
a=ssrc:1823804162 cname:pion1
a=ssrc:1823804162 msid:pion1 audio
a=ssrc:1823804162 mslabel:pion1
a=ssrc:1823804162 label:audio
a=msid:pion1 audio
m=video 9 UDP/TLS/RTP/SAVPF 105 115 135
c=IN IP4 0.0.0.0
a=mid:1
a=rtpmap:105 VP8/90000
a=rtpmap:115 H264/90000
a=fmtp:115 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f
a=rtpmap:135 VP9/90000
a=ssrc:2949882636 cname:pion2
a=ssrc:2949882636 msid:pion2 video
a=ssrc:2949882636 mslabel:pion2
a=ssrc:2949882636 label:video
a=msid:pion2 video
m=application 9 DTLS/SCTP 5000
c=IN IP4 0.0.0.0
a=mid:2
a=sctpmap:5000 webrtc-datachannel 1024
`

func TestPopulateFromSDP(t *testing.T) {
	m := MediaEngine{}
	assertCodecWithPayloadType := func(name string, payloadType uint8) {
		for _, c := range m.GetCodecsByName(name) {
			if c.PayloadType == payloadType && c.Name == name {
				return
			}
		}
		t.Fatalf("Failed to find codec(%s) with PayloadType(%d)", name, payloadType)
	}

	m.RegisterDefaultCodecs()
	assert.NoError(t, m.PopulateFromSDP(webrtc.SessionDescription{SDP: sdpValue}))

	assertCodecWithPayloadType(webrtc.Opus, 111)
	assertCodecWithPayloadType(webrtc.VP8, 105)
	assertCodecWithPayloadType(webrtc.H264, 115)
	assertCodecWithPayloadType(webrtc.VP9, 135)
}
