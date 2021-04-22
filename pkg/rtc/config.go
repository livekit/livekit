package rtc

import (
	"errors"
	"fmt"
	"net"

	"github.com/go-logr/zapr"
	"github.com/pion/ice/v2"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
)

type WebRTCConfig struct {
	Configuration webrtc.Configuration
	SettingEngine webrtc.SettingEngine
	Receiver      ReceiverConfig
	BufferFactory *buffer.Factory
	UDPMux        ice.UDPMux
}

type ReceiverConfig struct {
	packetBufferSize int
	maxBitrate       uint64
}

// number of packets to buffer up
const readBufferSize = 50

func NewWebRTCConfig(conf *config.RTCConfig, externalIP string) (*WebRTCConfig, error) {
	c := webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}
	s := webrtc.SettingEngine{}
	loggerFactory := logging.NewDefaultLoggerFactory()
	lkLogger := loggerFactory.NewLogger("livekit-mux")

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

	networkTypes := make([]webrtc.NetworkType, 0, 4)
	if !conf.ForceTCP {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeUDP4)
	}

	var udpMux *ice.UDPMuxDefault
	if conf.UDPPort != 0 {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{
			Port: int(conf.UDPPort),
		})
		if err != nil {
			return nil, err
		}
		udpMux = ice.NewUDPMuxDefault(ice.UDPMuxParams{
			Logger:  lkLogger,
			UDPConn: conn,
		})
		s.SetICEUDPMux(udpMux)
	} else if conf.ICEPortRangeStart != 0 && conf.ICEPortRangeEnd != 0 {
		if err := s.SetEphemeralUDPPortRange(conf.ICEPortRangeStart, conf.ICEPortRangeEnd); err != nil {
			return nil, err
		}
	}

	// use TCP mux when it's set
	if conf.TCPPort != 0 {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeTCP4,
		)
		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{
			Port: int(conf.TCPPort),
		})
		if err != nil {
			return nil, err
		}

		tcpMux := webrtc.NewICETCPMux(lkLogger, tcpListener, readBufferSize)
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
		UDPMux: udpMux,
	}, nil
}
