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

	protoCodecs "github.com/livekit/protocol/codecs"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
)

// flexFECRepairWindow is the flexfec-03 "repair-window" fmtp value in
// microseconds (10 s), matching pion's ConfigureFlexFEC03 default.
const flexFECRepairWindow = 10_000_000

// flexFECCodecParameters returns the flexfec-03 codec registered/offered when
// FlexFEC is enabled for a direction. Mirrors pion's ConfigureFlexFEC03 codec
// minus its generator interceptor (the SFU runs its own encode/decode paths).
func flexFECCodecParameters(payloadType uint8) webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeFlexFEC03,
			ClockRate:   90000,
			SDPFmtpLine: fmt.Sprintf("repair-window=%d", flexFECRepairWindow),
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBTransportCC},
			},
		},
		PayloadType: webrtc.PayloadType(payloadType),
	}
}

func isFlexFEC03MimeType(mimeType string) bool {
	return strings.EqualFold(mimeType, webrtc.MimeTypeFlexFEC03)
}

// validateFlexFECPayloadType ensures the configured flexfec payload type does
// not collide with any known codec payload type or its RTX (pt+1) slot.
func validateFlexFECPayloadType(payloadType uint8) error {
	pt := webrtc.PayloadType(payloadType)
	for _, codec := range protoCodecs.VideoCodecsParameters {
		if pt == codec.PayloadType || pt == codec.PayloadType+1 {
			return fmt.Errorf("flexfec payload type %d collides with %s (pt %d / rtx pt %d)",
				payloadType, codec.MimeType, codec.PayloadType, codec.PayloadType+1)
		}
	}
	for _, codec := range []webrtc.RTPCodecParameters{
		protoCodecs.OpusCodecParameters,
		protoCodecs.RedCodecParameters,
		protoCodecs.PCMUCodecParameters,
		protoCodecs.PCMACodecParameters,
	} {
		if pt == codec.PayloadType {
			return fmt.Errorf("flexfec payload type %d collides with %s (pt %d)",
				payloadType, codec.MimeType, codec.PayloadType)
		}
	}
	return nil
}

func registerCodecs(me *webrtc.MediaEngine, codecs []*livekit.Codec, rtcpFeedback RTCPFeedbackConfig, filterOutH264HighProfile bool) error {
	// audio codecs
	if IsCodecEnabled(codecs, protoCodecs.OpusCodecParameters.RTPCodecCapability) {
		cp := protoCodecs.OpusCodecParameters
		cp.RTPCodecCapability.RTCPFeedback = rtcpFeedback.Audio
		if err := me.RegisterCodec(cp, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}

		if IsCodecEnabled(codecs, protoCodecs.RedCodecParameters.RTPCodecCapability) {
			if err := me.RegisterCodec(protoCodecs.RedCodecParameters, webrtc.RTPCodecTypeAudio); err != nil {
				return err
			}
		}
	}

	for _, codec := range []webrtc.RTPCodecParameters{protoCodecs.PCMUCodecParameters, protoCodecs.PCMACodecParameters} {
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
	rtxEnabled := IsCodecEnabled(codecs, protoCodecs.VideoRTXCodecParameters.RTPCodecCapability)
	for _, codec := range protoCodecs.VideoCodecsParameters {
		if filterOutH264HighProfile && codec.RTPCodecCapability.SDPFmtpLine == protoCodecs.H264HighProfileFmtp {
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

		cp = protoCodecs.VideoRTXCodecParameters
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

	if config.FlexFEC.Enabled {
		// registering a flexfec codec makes pion allocate FEC SSRCs for video
		// senders and emit a=ssrc-group:FEC-FR in offers, and lets answers
		// accept flexfec offered by publishers
		if err := me.RegisterCodec(flexFECCodecParameters(config.FlexFEC.PayloadType), webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
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

// mergeCodecsByMime returns a union b, deduplicated by mime type.
func mergeCodecsByMime(a, b []*livekit.Codec) []*livekit.Codec {
	merged := make([]*livekit.Codec, 0, len(a)+len(b))
	merged = append(merged, a...)
	for _, c := range b {
		seen := false
		for _, existing := range a {
			if mime.IsMimeTypeStringEqual(c.Mime, existing.Mime) {
				seen = true
				break
			}
		}
		if !seen {
			merged = append(merged, c)
		}
	}
	return merged
}

func filterCodecs(
	codecs []webrtc.RTPCodecParameters,
	enabledCodecs []*livekit.Codec,
	rtcpFeedbackConfig RTCPFeedbackConfig,
	filterOutH264HighProfile bool,
	keepFlexFEC bool,
) []webrtc.RTPCodecParameters {
	filteredCodecs := make([]webrtc.RTPCodecParameters, 0, len(codecs))
	for _, c := range codecs {
		if filterOutH264HighProfile && isH264HighProfile(c.RTPCodecCapability.SDPFmtpLine) {
			continue
		}

		// flexfec-03 is not part of the enabled codec lists, retain it when
		// the transport direction has FlexFEC enabled
		if isFlexFEC03MimeType(c.RTPCodecCapability.MimeType) {
			if keepFlexFEC {
				filteredCodecs = append(filteredCodecs, c)
			}
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
