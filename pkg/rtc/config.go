package rtc

import (
	"errors"
	"fmt"
	"net"

	"github.com/go-logr/zapr"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
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
	bufferFactory := buffer.NewBufferFactory(conf.PacketBufferSize, zapr.NewLogger(logger.Desugar()))
	s.BufferFactory = bufferFactory.GetOrNew

	networkTypes := []webrtc.NetworkType{}

	if !conf.ForceTCP {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeUDP4,
			webrtc.NetworkTypeUDP6)
	}

	// use TCP mux when it's set
	if conf.ICETCPPort != 0 {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeTCP4,
			webrtc.NetworkTypeTCP6,
		)
		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{
			IP:   net.IP{0, 0, 0, 0},
			Port: int(conf.ICETCPPort),
		})
		if err != nil {
			return nil, err
		}

		tcpMux := webrtc.NewICETCPMux(nil, tcpListener, 10)
		s.SetICETCPMux(tcpMux)
	}

	if len(networkTypes) == 0 {
		return nil, errors.New("TCP is forced but not configured")
	}
	s.SetNetworkTypes(networkTypes)

	return &WebRTCConfig{
		Configuration: c,
		SettingEngine: s,
		BufferFactory: bufferFactory,
		Receiver: ReceiverConfig{
			packetBufferSize: conf.PacketBufferSize,
			maxBitrate:       conf.MaxBitrate,
		},
	}, nil
}
