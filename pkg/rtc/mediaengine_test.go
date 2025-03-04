// Copyright 2023 LiveKit, Inc.
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

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
)

func TestIsCodecEnabled(t *testing.T) {
	t.Run("empty fmtp requirement should match all", func(t *testing.T) {
		enabledCodecs := []*livekit.Codec{{Mime: "video/h264"}}
		require.True(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: mime.MimeTypeH264.String(), SDPFmtpLine: "special"}))
		require.True(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: mime.MimeTypeH264.String()}))
		require.False(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP8.String()}))
	})

	t.Run("when fmtp is provided, require match", func(t *testing.T) {
		enabledCodecs := []*livekit.Codec{{Mime: "video/h264", FmtpLine: "special"}}
		require.True(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: mime.MimeTypeH264.String(), SDPFmtpLine: "special"}))
		require.False(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: mime.MimeTypeH264.String()}))
		require.False(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP8.String()}))
	})
}
