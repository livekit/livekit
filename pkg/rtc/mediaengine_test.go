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
	"strconv"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
)

const (
	h264MainProfilePacketizationMode0Fmtp     = "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=4d001f"
	h264MainProfilePacketizationMode1Fmtp     = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"
	h264BaselineProfilePacketizationMode0Fmtp = "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f"
	h264BaselineProfilePacketizationMode1Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"
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

	t.Run("strict fmtp requires an exact fmtp, mime alone is not enough", func(t *testing.T) {
		cap := webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeH264.String(),
			SDPFmtpLine: h264MainProfilePacketizationMode1Fmtp,
		}

		// mime-only config opts into every non-strict codec, but never a strict one
		mimeOnly := []*livekit.Codec{{Mime: "video/h264"}}
		require.True(t, isCodecEnabledWithFmtp(mimeOnly, cap, false))
		require.False(t, isCodecEnabledWithFmtp(mimeOnly, cap, true))

		// spelling the fmtp out enables it
		explicit := []*livekit.Codec{{Mime: "video/h264", FmtpLine: h264MainProfilePacketizationMode1Fmtp}}
		require.True(t, isCodecEnabledWithFmtp(explicit, cap, true))

		// ...but only for the profile that was asked for
		require.False(t, isCodecEnabledWithFmtp(explicit, webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeH264.String(),
			SDPFmtpLine: h264MainProfilePacketizationMode0Fmtp,
		}, true))
	})
}

type offeredCodec struct {
	payloadType webrtc.PayloadType
	name        string
	fmtp        string
}

func (o offeredCodec) String() string {
	return strconv.Itoa(int(o.payloadType)) + " " + o.name + " " + o.fmtp
}

// offeredVideoCodecs returns the video codecs an offer built from enabledCodecs carries.
// It goes through a real offer rather than inspecting the MediaEngine so that payload
// type assignment, which is what collides, is covered too.
func offeredVideoCodecs(t *testing.T, enabledCodecs []*livekit.Codec, filterOutH264HighProfile bool) []offeredCodec {
	t.Helper()

	me, err := createMediaEngine(enabledCodecs, DirectionConfig{}, filterOutH264HighProfile)
	require.NoError(t, err)

	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	defer pc.Close()

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	require.NoError(t, err)

	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)

	var codecs []offeredCodec
	indexByPayloadType := make(map[webrtc.PayloadType]int)
	for _, line := range strings.Split(offer.SDP, "\r\n") {
		attr, value, found := strings.Cut(line, ":")
		if !found || (attr != "a=rtpmap" && attr != "a=fmtp") {
			continue
		}

		rawPayloadType, rest, _ := strings.Cut(value, " ")
		payloadType, err := strconv.Atoi(rawPayloadType)
		require.NoError(t, err)

		if attr == "a=rtpmap" {
			indexByPayloadType[webrtc.PayloadType(payloadType)] = len(codecs)
			codecs = append(codecs, offeredCodec{payloadType: webrtc.PayloadType(payloadType), name: rest})
			continue
		}

		i, ok := indexByPayloadType[webrtc.PayloadType(payloadType)]
		require.True(t, ok, "fmtp for payload type %d without an rtpmap", payloadType)
		codecs[i].fmtp = rest
	}

	require.NotEmpty(t, codecs)
	return codecs
}

func hasOfferedFmtp(codecs []offeredCodec, mimeType mime.MimeType, fmtp string) bool {
	for _, c := range codecs {
		name, _, _ := strings.Cut(c.name, "/")
		if mime.NormalizeMimeType("video/"+name) == mimeType && c.fmtp == fmtp {
			return true
		}
	}
	return false
}

// defaultEnabledCodecs mirrors what roomallocator derives from config, so these tests
// track the shipped default codec set instead of a hand-maintained copy of it.
func defaultEnabledCodecs() []*livekit.Codec {
	codecs := make([]*livekit.Codec, 0, len(config.DefaultConfig.Room.EnabledCodecs))
	for _, c := range config.DefaultConfig.Room.EnabledCodecs {
		codecs = append(codecs, &livekit.Codec{Mime: c.Mime, FmtpLine: c.FmtpLine})
	}
	return codecs
}

func TestRegisterCodecsH264Profiles(t *testing.T) {
	t.Run("constrained baseline are offered by default", func(t *testing.T) {
		codecs := offeredVideoCodecs(t, defaultEnabledCodecs(), false)

		for _, fmtp := range []string{
			"level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f",
			"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640032",
		} {
			require.True(t, hasOfferedFmtp(codecs, mime.MimeTypeH264, fmtp), "missing %q in %v", fmtp, codecs)
		}
	})

	t.Run("main/baseline profile is not offered unless explicitly enabled", func(t *testing.T) {
		codecs := offeredVideoCodecs(t, defaultEnabledCodecs(), false)

		require.False(t, hasOfferedFmtp(codecs, mime.MimeTypeH264, h264MainProfilePacketizationMode0Fmtp), "%v", codecs)
		require.False(t, hasOfferedFmtp(codecs, mime.MimeTypeH264, h264MainProfilePacketizationMode1Fmtp), "%v", codecs)
		require.False(t, hasOfferedFmtp(codecs, mime.MimeTypeH264, h264BaselineProfilePacketizationMode0Fmtp), "%v", codecs)
		require.False(t, hasOfferedFmtp(codecs, mime.MimeTypeH264, h264BaselineProfilePacketizationMode1Fmtp), "%v", codecs)
	})

	t.Run("main/baseline profile is offered when explicitly enabled", func(t *testing.T) {
		for _, fmtp := range []string{
			h264MainProfilePacketizationMode0Fmtp,
			h264MainProfilePacketizationMode1Fmtp,
			h264BaselineProfilePacketizationMode0Fmtp,
			h264BaselineProfilePacketizationMode1Fmtp,
		} {
			t.Run(fmtp, func(t *testing.T) {
				enabled := append(defaultEnabledCodecs(), &livekit.Codec{
					Mime:     mime.MimeTypeH264.String(),
					FmtpLine: fmtp,
				})

				codecs := offeredVideoCodecs(t, enabled, false)
				require.True(t, hasOfferedFmtp(codecs, mime.MimeTypeH264, fmtp), "missing %q in %v", fmtp, codecs)
			})
		}
	})

	t.Run("high profile is filtered out for the answerer", func(t *testing.T) {
		highProfileFmtp := "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640032"

		codecs := offeredVideoCodecs(t, defaultEnabledCodecs(), true)
		require.False(t, hasOfferedFmtp(codecs, mime.MimeTypeH264, highProfileFmtp), "%v", codecs)

		// the other profiles survive the filter
		require.True(t, hasOfferedFmtp(codecs, mime.MimeTypeH264,
			"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"), "%v", codecs)
	})
}

// Every video codec claims its own payload type plus the next one for its RTX stream,
// so a codec whose payload type lands on another's RTX slot makes pion refuse to build
// the MediaEngine at all.
func TestRegisterCodecsPayloadTypesAreUnique(t *testing.T) {
	enabled := append(defaultEnabledCodecs(),
		&livekit.Codec{Mime: mime.MimeTypeH264.String(), FmtpLine: h264MainProfilePacketizationMode0Fmtp},
		&livekit.Codec{Mime: mime.MimeTypeH264.String(), FmtpLine: h264MainProfilePacketizationMode1Fmtp},
	)

	for _, filterOutH264HighProfile := range []bool{false, true} {
		codecs := offeredVideoCodecs(t, enabled, filterOutH264HighProfile)

		seen := make(map[webrtc.PayloadType]offeredCodec, len(codecs))
		for _, c := range codecs {
			previous, duplicate := seen[c.payloadType]
			require.False(t, duplicate, "payload type %d claimed by both %v and %v", c.payloadType, previous, c)
			seen[c.payloadType] = c
		}
	}
}
