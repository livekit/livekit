package rtc

import (
	"fmt"

	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
)

type WebRTCConfig struct {
	Configuration webrtc.Configuration
	SettingEngine webrtc.SettingEngine
	Receiver      ReceiverConfig
	BufferFactory *buffer.Factory
}

type ReceiverConfig struct {
	packetBufferSize int
	maxBitrate       uint64
	maxBufferTime    int
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
	if conf.UseExternalIP && externalIP != "" {
		s.SetNAT1To1IPs([]string{externalIP}, webrtc.ICECandidateTypeHost)
	}

	if conf.PacketBufferSize == 0 {
		conf.PacketBufferSize = 500
	}
	bufferFactory := buffer.NewBufferFactory(conf.PacketBufferSize)
	s.BufferFactory = bufferFactory.GetOrNew

	return &WebRTCConfig{
		Configuration: c,
		SettingEngine: s,
		BufferFactory: bufferFactory,
		Receiver: ReceiverConfig{
			packetBufferSize: conf.PacketBufferSize,
			maxBitrate:       conf.MaxBitrate,
			maxBufferTime:    conf.MaxBufferTime,
		},
	}, nil
}
