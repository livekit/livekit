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

func TestBindableCodecs(t *testing.T) {
	codec := func(m mime.MimeType) webrtc.RTPCodecParameters {
		return webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: m.String()}}
	}
	red, opus := codec(mime.MimeTypeRED), codec(mime.MimeTypeOpus)
	vp9, vp8 := codec(mime.MimeTypeVP9), codec(mime.MimeTypeVP8)

	cases := []struct {
		name      string
		encrypted bool
		upstream  []webrtc.RTPCodecParameters
		want      []webrtc.RTPCodecParameters
	}{
		{name: "RED and Opus", upstream: []webrtc.RTPCodecParameters{red, opus}, want: []webrtc.RTPCodecParameters{red, opus}},
		{name: "encrypted RED and Opus", encrypted: true, upstream: []webrtc.RTPCodecParameters{red, opus}, want: []webrtc.RTPCodecParameters{red}},
		{name: "encrypted Opus and RED", encrypted: true, upstream: []webrtc.RTPCodecParameters{opus, red}, want: []webrtc.RTPCodecParameters{opus}},
		{name: "encrypted video regression", encrypted: true, upstream: []webrtc.RTPCodecParameters{vp9, vp8}, want: []webrtc.RTPCodecParameters{vp9, vp8}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &DownTrack{params: DownTrackParams{IsEncrypted: c.encrypted}, upstreamCodecs: c.upstream}
			require.Equal(t, c.want, d.BindableCodecs())
		})
	}
}
