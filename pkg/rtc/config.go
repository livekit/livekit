package rtc

import (
	"fmt"
	"net/url"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
)

type WebRTCConfig struct {
	configuration webrtc.Configuration
	setting       webrtc.SettingEngine
	feedbackTypes []webrtc.RTCPFeedback

	receiver ReceiverConfig
}

type ReceiverConfig struct {
	maxBandwidth  uint64
	maxBufferTime int
}

func NewWebRTCConfig(conf *config.RTCConfig, externalIP string) (*WebRTCConfig, error) {
	c := webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}
	s := webrtc.SettingEngine{}
	ft := []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBCCM},
		{Type: webrtc.TypeRTCPFBNACK},
		{Type: webrtc.TypeRTCPFBGoogREMB},
		{Type: "nack pli"},
	}

	if conf.ICEPortRangeStart != 0 && conf.ICEPortRangeEnd != 0 {
		if err := s.SetEphemeralUDPPortRange(conf.ICEPortRangeStart, conf.ICEPortRangeEnd); err != nil {
			return nil, err
		}
	}

	iceUrls := make([]string, 0)
	for _, stunServer := range conf.StunServers {
		iceUrls = append(iceUrls, fmt.Sprintf("stun:%s", stunServer))
	}
	c.ICEServers = []webrtc.ICEServer{
		{
			URLs: iceUrls,
		},
	}
	if conf.UseExternalIP {
		s.SetNAT1To1IPs([]string{externalIP}, webrtc.ICECandidateTypeHost)
	}

	// Configure required extensions
	sdes, _ := url.Parse(sdp.SDESRTPStreamIDURI)
	sdedMid, _ := url.Parse(sdp.SDESMidURI)
	exts := []sdp.ExtMap{
		{
			URI: sdes,
		},
		{
			URI: sdedMid,
		},
	}
	s.AddSDPExtensions(webrtc.SDPSectionVideo, exts)

	return &WebRTCConfig{
		configuration: c,
		setting:       s,
		feedbackTypes: ft,
		receiver: ReceiverConfig{
			maxBandwidth:  conf.MaxBandwidth,
			maxBufferTime: conf.MaxBufferTime,
		},
	}, nil
}
