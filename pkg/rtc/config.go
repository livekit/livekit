package rtc

import (
	"fmt"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
)

type WebRTCConfig struct {
	Configuration webrtc.Configuration
	SettingEngine webrtc.SettingEngine

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

	return &WebRTCConfig{
		Configuration: c,
		SettingEngine: s,
		receiver: ReceiverConfig{
			maxBandwidth:  conf.MaxBandwidth,
			maxBufferTime: conf.MaxBufferTime,
		},
	}, nil
}
