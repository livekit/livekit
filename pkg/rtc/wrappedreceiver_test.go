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

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	protoCodecs "github.com/livekit/protocol/codecs"
	"github.com/livekit/protocol/codecs/mime"
)

func TestWrappedReceiverBindableCodecs(t *testing.T) {
	red, opus := protoCodecs.RedCodecParameters, protoCodecs.OpusCodecParameters

	cases := []struct {
		name      string
		encrypted bool
		upstream  []webrtc.RTPCodecParameters
		want      []mime.MimeType
	}{
		{name: "Opus, RED added", upstream: []webrtc.RTPCodecParameters{opus}, want: []mime.MimeType{mime.MimeTypeRED, mime.MimeTypeOpus}},
		{name: "encrypted Opus, added RED excluded", encrypted: true, upstream: []webrtc.RTPCodecParameters{opus}, want: []mime.MimeType{mime.MimeTypeOpus}},
		{name: "encrypted RED, added Opus excluded", encrypted: true, upstream: []webrtc.RTPCodecParameters{red}, want: []mime.MimeType{mime.MimeTypeRED}},
		{name: "encrypted RED and Opus both published", encrypted: true, upstream: []webrtc.RTPCodecParameters{red, opus}, want: []mime.MimeType{mime.MimeTypeRED, mime.MimeTypeOpus}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wr := NewWrappedReceiver(WrappedReceiverParams{UpstreamCodecs: c.upstream, IsEncrypted: c.encrypted})
			var got []mime.MimeType
			for _, codec := range wr.BindableCodecs() {
				got = append(got, mime.NormalizeMimeType(codec.MimeType))
			}
			require.Equal(t, c.want, got)
		})
	}
}
