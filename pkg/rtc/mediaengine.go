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
	"fmt"
	"strings"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
)

var (
	OpusCodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeOpus.String(),
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}

	RedCodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeRED.String(),
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "111/111",
		},
		PayloadType: 63,
	}

	PCMUCodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mime.MimeTypePCMU.String(),
			ClockRate: 8000,
		},
		PayloadType: 0,
	}

	PCMACodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mime.MimeTypePCMA.String(),
			ClockRate: 8000,
		},
		PayloadType: 8,
	}

	videoRTXCodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mime.MimeTypeRTX.String(),
			ClockRate: 90000,
		},
	}

	vp8CodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mime.MimeTypeVP8.String(),
			ClockRate: 90000,
		},
		PayloadType: 96,
	}

	vp9ProfileId0CodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeVP9.String(),
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=0",
		},
		PayloadType: 98,
	}

	vp9ProfileId1CodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeVP9.String(),
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=1",
		},
		PayloadType: 100,
	}

	h264ProfileLevelId42e01fPacketizationMode0CodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeH264.String(),
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		PayloadType: 125,
	}

	h264ProfileLevelId42e01fPacketizationMode1CodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeH264.String(),
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f",
		},
		PayloadType: 108,
	}

	h264HighProfileFmtp            = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640032"
	h264HighProfileCodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mime.MimeTypeH264.String(),
			ClockRate:   90000,
			SDPFmtpLine: h264HighProfileFmtp,
		},
		PayloadType: 123,
	}

	av1CodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mime.MimeTypeAV1.String(),
			ClockRate: 90000,
		},
		PayloadType: 35,
	}

	h265CodecParameters = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mime.MimeTypeH265.String(),
			ClockRate: 90000,
		},
		PayloadType: 116,
	}

	videoCodecsParameters = []webrtc.RTPCodecParameters{
		vp8CodecParameters,
		vp9ProfileId0CodecParameters,
		vp9ProfileId1CodecParameters,
		h264ProfileLevelId42e01fPacketizationMode0CodecParameters,
		h264ProfileLevelId42e01fPacketizationMode1CodecParameters,
		h264HighProfileCodecParameters,
		av1CodecParameters,
		h265CodecParameters,
	}
)

func registerCodecs(me *webrtc.MediaEngine, codecs []*livekit.Codec, rtcpFeedback RTCPFeedbackConfig, filterOutH264HighProfile bool) error {
	// audio codecs
	if IsCodecEnabled(codecs, OpusCodecParameters.RTPCodecCapability) {
		cp := OpusCodecParameters
		cp.RTPCodecCapability.RTCPFeedback = rtcpFeedback.Audio
		if err := me.RegisterCodec(cp, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}

		if IsCodecEnabled(codecs, RedCodecParameters.RTPCodecCapability) {
			if err := me.RegisterCodec(RedCodecParameters, webrtc.RTPCodecTypeAudio); err != nil {
				return err
			}
		}
	}

	for _, codec := range []webrtc.RTPCodecParameters{PCMUCodecParameters, PCMACodecParameters} {
		if !IsCodecEnabled(codecs, codec.RTPCodecCapability) {
			continue
		}

		cp := codec
		cp.RTPCodecCapability.RTCPFeedback = rtcpFeedback.Audio
		if err := me.RegisterCodec(cp, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}

	// video codecs
	rtxEnabled := IsCodecEnabled(codecs, videoRTXCodecParameters.RTPCodecCapability)
	for _, codec := range videoCodecsParameters {
		if filterOutH264HighProfile && codec.RTPCodecCapability.SDPFmtpLine == h264HighProfileFmtp {
			continue
		}
		if mime.IsMimeTypeStringRTX(codec.MimeType) {
			continue
		}
		if !IsCodecEnabled(codecs, codec.RTPCodecCapability) {
			continue
		}

		cp := codec
		cp.RTPCodecCapability.RTCPFeedback = rtcpFeedback.Video
		if err := me.RegisterCodec(cp, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}

		if !rtxEnabled {
			continue
		}

		cp = videoRTXCodecParameters
		cp.RTPCodecCapability.SDPFmtpLine = fmt.Sprintf("apt=%d", codec.PayloadType)
		cp.PayloadType = codec.PayloadType + 1
		if err := me.RegisterCodec(cp, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	return nil
}

func registerHeaderExtensions(me *webrtc.MediaEngine, rtpHeaderExtension RTPHeaderExtensionConfig) error {
	for _, extension := range rtpHeaderExtension.Video {
		if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: extension}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}

	for _, extension := range rtpHeaderExtension.Audio {
		if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: extension}, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}

	return nil
}

func createMediaEngine(codecs []*livekit.Codec, config DirectionConfig, filterOutH264HighProfile bool) (*webrtc.MediaEngine, error) {
	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me, codecs, config.RTCPFeedback, filterOutH264HighProfile); err != nil {
		return nil, err
	}

	if err := registerHeaderExtensions(me, config.RTPHeaderExtension); err != nil {
		return nil, err
	}

	return me, nil
}

func IsCodecEnabled(codecs []*livekit.Codec, cap webrtc.RTPCodecCapability) bool {
	for _, codec := range codecs {
		if !mime.IsMimeTypeStringEqual(codec.Mime, cap.MimeType) {
			continue
		}
		if codec.FmtpLine == "" || strings.EqualFold(codec.FmtpLine, cap.SDPFmtpLine) {
			return true
		}
	}
	return false
}

func selectAlternativeVideoCodec(enabledCodecs []*livekit.Codec) string {
	for _, c := range enabledCodecs {
		if mime.IsMimeTypeStringVideo(c.Mime) {
			return c.Mime
		}
	}
	// no viable codec in the list of enabled codecs, fall back to the most widely supported codec
	return mime.MimeTypeVP8.String()
}

func selectAlternativeAudioCodec(enabledCodecs []*livekit.Codec) string {
	for _, c := range enabledCodecs {
		if mime.IsMimeTypeStringAudio(c.Mime) {
			return c.Mime
		}
	}
	// no viable codec in the list of enabled codecs, fall back to the most widely supported codec
	return mime.MimeTypeOpus.String()
}

func filterCodecs(
	codecs []webrtc.RTPCodecParameters,
	enabledCodecs []*livekit.Codec,
	rtcpFeedbackConfig RTCPFeedbackConfig,
	filterOutH264HighProfile bool,
) []webrtc.RTPCodecParameters {
	filteredCodecs := make([]webrtc.RTPCodecParameters, 0, len(codecs))
	for _, c := range codecs {
		if filterOutH264HighProfile && isH264HighProfile(c.RTPCodecCapability.SDPFmtpLine) {
			continue
		}

		for _, enabledCodec := range enabledCodecs {
			if mime.NormalizeMimeType(enabledCodec.Mime) == mime.NormalizeMimeType(c.RTPCodecCapability.MimeType) {
				if !mime.IsMimeTypeStringEqual(c.RTPCodecCapability.MimeType, mime.MimeTypeRTX.String()) {
					if mime.IsMimeTypeStringVideo(c.RTPCodecCapability.MimeType) {
						c.RTPCodecCapability.RTCPFeedback = rtcpFeedbackConfig.Video
					} else {
						c.RTPCodecCapability.RTCPFeedback = rtcpFeedbackConfig.Audio
					}
				}
				filteredCodecs = append(filteredCodecs, c)
				break
			}
		}
	}
	return filteredCodecs
}

func isH264HighProfile(fmtp string) bool {
	params := strings.Split(fmtp, ";")
	for _, param := range params {
		parts := strings.Split(param, "=")
		if len(parts) == 2 {
			if parts[0] == "profile-level-id" {
				// https://datatracker.ietf.org/doc/html/rfc6184#section-8.1
				// hex value 0x64 for profile_idc is high profile
				return strings.HasPrefix(parts[1], "64")
			}
		}
	}

	return false
}
