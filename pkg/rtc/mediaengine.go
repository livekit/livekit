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

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/protocol/livekit"
)

var OpusCodecCapability = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeOpus,
	ClockRate:   48000,
	Channels:    2,
	SDPFmtpLine: "minptime=10;useinbandfec=1",
}
var RedCodecCapability = webrtc.RTPCodecCapability{
	MimeType:    sfu.MimeTypeAudioRed,
	ClockRate:   48000,
	Channels:    2,
	SDPFmtpLine: "111/111",
}
var videoRTX = webrtc.RTPCodecCapability{
	MimeType:  webrtc.MimeTypeRTX,
	ClockRate: 90000,
}

func registerCodecs(me *webrtc.MediaEngine, codecs []*livekit.Codec, rtcpFeedback RTCPFeedbackConfig, filterOutH264HighProfile bool) error {
	opusCodec := OpusCodecCapability
	opusCodec.RTCPFeedback = rtcpFeedback.Audio
	var opusPayload webrtc.PayloadType
	if IsCodecEnabled(codecs, opusCodec) {
		opusPayload = 111
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: opusCodec,
			PayloadType:        opusPayload,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}

		if IsCodecEnabled(codecs, RedCodecCapability) {
			if err := me.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: RedCodecCapability,
				PayloadType:        63,
			}, webrtc.RTPCodecTypeAudio); err != nil {
				return err
			}
		}
	}

	rtxEnabled := IsCodecEnabled(codecs, videoRTX)

	h264HighProfileFmtp := "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640032"
	for _, codec := range []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeVP8,
				ClockRate:    90000,
				RTCPFeedback: rtcpFeedback.Video,
			},
			PayloadType: 96,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeVP9,
				ClockRate:    90000,
				SDPFmtpLine:  "profile-id=0",
				RTCPFeedback: rtcpFeedback.Video,
			},
			PayloadType: 98,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeVP9,
				ClockRate:    90000,
				SDPFmtpLine:  "profile-id=1",
				RTCPFeedback: rtcpFeedback.Video,
			},
			PayloadType: 100,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
				RTCPFeedback: rtcpFeedback.Video,
			},
			PayloadType: 125,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f",
				RTCPFeedback: rtcpFeedback.Video,
			},
			PayloadType: 108,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  h264HighProfileFmtp,
				RTCPFeedback: rtcpFeedback.Video,
			},
			PayloadType: 123,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeAV1,
				ClockRate:    90000,
				RTCPFeedback: rtcpFeedback.Video,
			},
			PayloadType: 35,
		},
	} {
		if filterOutH264HighProfile && codec.RTPCodecCapability.SDPFmtpLine == h264HighProfileFmtp {
			continue
		}
		if strings.EqualFold(codec.MimeType, webrtc.MimeTypeRTX) {
			continue
		}
		if IsCodecEnabled(codecs, codec.RTPCodecCapability) {
			if err := me.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
				return err
			}
			if rtxEnabled {
				if err := me.RegisterCodec(webrtc.RTPCodecParameters{
					RTPCodecCapability: webrtc.RTPCodecCapability{
						MimeType:    webrtc.MimeTypeRTX,
						ClockRate:   90000,
						SDPFmtpLine: fmt.Sprintf("apt=%d", codec.PayloadType),
					},
					PayloadType: codec.PayloadType + 1,
				}, webrtc.RTPCodecTypeVideo); err != nil {
					return err
				}
			}
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
		if !strings.EqualFold(codec.Mime, cap.MimeType) {
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
		if strings.HasPrefix(c.Mime, "video/") {
			return c.Mime
		}
	}
	// no viable codec in the list of enabled codecs, fall back to the most widely supported codec
	return webrtc.MimeTypeVP8
}
