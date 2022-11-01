package rtc

import (
	"strings"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/protocol/livekit"
)

var opusCodecCapability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}
var redCodecCapability = webrtc.RTPCodecCapability{MimeType: sfu.MimeTypeAudioRed, ClockRate: 48000, Channels: 2, SDPFmtpLine: "111/111"}

func registerCodecs(me *webrtc.MediaEngine, codecs []*livekit.Codec, rtcpFeedback RTCPFeedbackConfig) error {
	opusCodec := opusCodecCapability
	opusCodec.RTCPFeedback = rtcpFeedback.Audio
	var opusPayload webrtc.PayloadType
	if isCodecEnabled(codecs, opusCodec) {
		opusPayload = 111
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: opusCodec,
			PayloadType:        opusPayload,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}

		if isCodecEnabled(codecs, redCodecCapability) {
			if err := me.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: redCodecCapability,
				PayloadType:        63,
			}, webrtc.RTPCodecTypeAudio); err != nil {
				return err
			}
		}
	}

	for _, codec := range []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: rtcpFeedback.Video},
			PayloadType:        96,
		},
		/*
			{
				RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0", RTCPFeedback: rtcpFeedback.Video},
				PayloadType:        98,
			},
			{
				RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=1", RTCPFeedback: rtcpFeedback.Video},
				PayloadType:        100,
			},
		*/
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", RTCPFeedback: rtcpFeedback.Video},
			PayloadType:        125,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f", RTCPFeedback: rtcpFeedback.Video},
			PayloadType:        108,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640032", RTCPFeedback: rtcpFeedback.Video},
			PayloadType:        123,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1, ClockRate: 90000, RTCPFeedback: rtcpFeedback.Video},
			PayloadType:        35,
		},
	} {
		if isCodecEnabled(codecs, codec.RTPCodecCapability) {
			if err := me.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
				return err
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

func createMediaEngine(codecs []*livekit.Codec, config DirectionConfig) (*webrtc.MediaEngine, error) {
	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me, codecs, config.RTCPFeedback); err != nil {
		return nil, err
	}

	if err := registerHeaderExtensions(me, config.RTPHeaderExtension); err != nil {
		return nil, err
	}

	return me, nil
}

func isCodecEnabled(codecs []*livekit.Codec, cap webrtc.RTPCodecCapability) bool {
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
