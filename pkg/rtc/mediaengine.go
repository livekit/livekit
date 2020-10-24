package rtc

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

const (
	mediaNameAudio = "audio"
	mediaNameVideo = "video"
)

// MediaEngine handles stream codecs
type MediaEngine struct {
	webrtc.MediaEngine
	feedbackTypes []webrtc.RTCPFeedback
	tCCExt        int
}

// PopulateFromSDP finds all codecs in sd and adds them to m, using the dynamic
// payload types and parameters from sd.
// PopulateFromSDP is intended for use when answering a request.
// The offerer sets the PayloadTypes for the connection.
// PopulateFromSDP allows an answerer to properly match the PayloadTypes from the offerer.
// A MediaEngine populated by PopulateFromSDP should be used only for a single session.
func (e *MediaEngine) PopulateFromSDP(sd webrtc.SessionDescription) error {
	s := sdp.SessionDescription{}
	if err := s.Unmarshal([]byte(sd.SDP)); err != nil {
		return err
	}

	for _, md := range s.MediaDescriptions {
		if md.MediaName.Media != mediaNameAudio && md.MediaName.Media != mediaNameVideo {
			continue
		}

		for _, att := range md.Attributes {
			if att.Key == sdp.AttrKeyExtMap && strings.HasSuffix(att.Value, sdp.TransportCCURI) {
				e.tCCExt, _ = strconv.Atoi(att.Value[:1])
				break
			}
		}

		for _, format := range md.MediaName.Formats {
			pt, err := strconv.Atoi(format)
			if err != nil {
				return fmt.Errorf("format parse error")
			}

			payloadType := uint8(pt)
			payloadCodec, err := s.GetCodecForPayloadType(payloadType)
			if err != nil {
				return fmt.Errorf("could not find codec for payload type %d", payloadType)
			}

			var codec *webrtc.RTPCodec
			switch {
			case strings.EqualFold(payloadCodec.Name, webrtc.Opus):
				codec = webrtc.NewRTPOpusCodec(payloadType, payloadCodec.ClockRate)
			case strings.EqualFold(payloadCodec.Name, webrtc.VP8):
				codec = webrtc.NewRTPVP8CodecExt(payloadType, payloadCodec.ClockRate, e.feedbackTypes, payloadCodec.Fmtp)
			case strings.EqualFold(payloadCodec.Name, webrtc.VP9):
				codec = webrtc.NewRTPVP9CodecExt(payloadType, payloadCodec.ClockRate, e.feedbackTypes, payloadCodec.Fmtp)
			case strings.EqualFold(payloadCodec.Name, webrtc.H264):
				codec = webrtc.NewRTPH264CodecExt(payloadType, payloadCodec.ClockRate, e.feedbackTypes, payloadCodec.Fmtp)
			default:
				// ignoring other codecs
				continue
			}

			e.RegisterCodec(codec)
		}
	}

	// Use defaults for codecs not provided in sdp
	if len(e.GetCodecsByName(webrtc.Opus)) == 0 {
		codec := webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000)
		e.RegisterCodec(codec)
	}

	if len(e.GetCodecsByName(webrtc.VP8)) == 0 {
		codec := webrtc.NewRTPVP8CodecExt(webrtc.DefaultPayloadTypeVP8, 90000, e.feedbackTypes, "")
		e.RegisterCodec(codec)
	}

	if len(e.GetCodecsByName(webrtc.VP9)) == 0 {
		codec := webrtc.NewRTPVP9CodecExt(webrtc.DefaultPayloadTypeVP9, 90000, e.feedbackTypes, "")
		e.RegisterCodec(codec)
	}

	if len(e.GetCodecsByName(webrtc.H264)) == 0 {
		codec := webrtc.NewRTPH264CodecExt(webrtc.DefaultPayloadTypeH264, 90000, e.feedbackTypes, "")
		e.RegisterCodec(codec)
	}

	return nil
}
