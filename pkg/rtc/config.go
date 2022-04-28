package rtc

import (
	"errors"
	"net"

	"github.com/pion/ice/v2"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/logger"

	dd "github.com/livekit/livekit-server/pkg/sfu/buffer/dependencydescriptor"

	"github.com/livekit/livekit-server/pkg/config"
	logging "github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	minUDPBufferSize     = 5_000_000
	defaultUDPBufferSize = 16_777_216
	frameMarking         = "urn:ietf:params:rtp-hdrext:framemarking"
)

type WebRTCConfig struct {
	Configuration  webrtc.Configuration
	SettingEngine  webrtc.SettingEngine
	Receiver       ReceiverConfig
	BufferFactory  *buffer.Factory
	UDPMux         ice.UDPMux
	UDPMuxConn     *net.UDPConn
	TCPMuxListener *net.TCPListener
	Publisher      DirectionConfig
	Subscriber     DirectionConfig
}

type ReceiverConfig struct {
	PacketBufferSize int
	maxBitrate       uint64
}

type RTPHeaderExtensionConfig struct {
	Audio []string
	Video []string
}

type RTCPFeedbackConfig struct {
	Audio []webrtc.RTCPFeedback
	Video []webrtc.RTCPFeedback
}

type DirectionConfig struct {
	RTPHeaderExtension RTPHeaderExtensionConfig
	RTCPFeedback       RTCPFeedbackConfig
}

// number of packets to buffer up
const readBufferSize = 50

func NewWebRTCConfig(conf *config.Config, externalIP string) (*WebRTCConfig, error) {
	rtcConf := conf.RTC
	c := webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}
	s := webrtc.SettingEngine{
		LoggerFactory: logging.NewLoggerFactory(logger.GetLogger()),
	}

	if conf.RTC.UseExternalIP && externalIP != "" {
		s.SetNAT1To1IPs([]string{externalIP}, webrtc.ICECandidateTypeHost)
	}

	if rtcConf.PacketBufferSize == 0 {
		rtcConf.PacketBufferSize = 500
	}

	var udpMux *ice.UDPMuxDefault
	var udpMuxConn *net.UDPConn
	var err error
	networkTypes := make([]webrtc.NetworkType, 0, 4)

	if !rtcConf.ForceTCP {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6,
		)
		if rtcConf.ICEPortRangeStart != 0 && rtcConf.ICEPortRangeEnd != 0 {
			if err := s.SetEphemeralUDPPortRange(uint16(rtcConf.ICEPortRangeStart), uint16(rtcConf.ICEPortRangeEnd)); err != nil {
				return nil, err
			}
		} else if rtcConf.UDPPort != 0 {
			udpMuxConn, err = net.ListenUDP("udp", &net.UDPAddr{
				Port: int(rtcConf.UDPPort),
			})
			if err != nil {
				return nil, err
			}
			_ = udpMuxConn.SetReadBuffer(defaultUDPBufferSize)
			_ = udpMuxConn.SetWriteBuffer(defaultUDPBufferSize)

			udpMux = ice.NewUDPMuxDefault(ice.UDPMuxParams{
				Logger:  s.LoggerFactory.NewLogger("udp_mux"),
				UDPConn: udpMuxConn,
			})
			s.SetICEUDPMux(udpMux)
			if !conf.Development {
				checkUDPReadBuffer()
			}
		}
	}

	// use TCP mux when it's set
	var tcpListener *net.TCPListener
	if rtcConf.TCPPort != 0 {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
		)
		tcpListener, err = net.ListenTCP("tcp", &net.TCPAddr{
			Port: int(rtcConf.TCPPort),
		})
		if err != nil {
			return nil, err
		}

		tcpMux := webrtc.NewICETCPMux(
			s.LoggerFactory.NewLogger("tcp_mux"),
			tcpListener,
			readBufferSize,
		)
		s.SetICETCPMux(tcpMux)
	}

	if len(networkTypes) == 0 {
		return nil, errors.New("TCP is forced but not configured")
	}
	s.SetNetworkTypes(networkTypes)

	// publisher configuration
	publisherConfig := DirectionConfig{
		RTPHeaderExtension: RTPHeaderExtensionConfig{
			Audio: []string{
				sdp.SDESMidURI,
				sdp.SDESRTPStreamIDURI,
				sdp.AudioLevelURI,
			},
			Video: []string{
				sdp.SDESMidURI,
				sdp.SDESRTPStreamIDURI,
				sdp.TransportCCURI,
				frameMarking,
				dd.ExtensionUrl,
			},
		},
		RTCPFeedback: RTCPFeedbackConfig{
			Video: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBGoogREMB},
				{Type: webrtc.TypeRTCPFBTransportCC},
				{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
				{Type: webrtc.TypeRTCPFBNACK},
				{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
			},
		},
	}

	// subscriber configuration
	subscriberConfig := DirectionConfig{
		RTPHeaderExtension: RTPHeaderExtensionConfig{
			Video: []string{dd.ExtensionUrl},
		},
		RTCPFeedback: RTCPFeedbackConfig{
			Video: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
				{Type: webrtc.TypeRTCPFBNACK},
				{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
			},
		},
	}
	if rtcConf.CongestionControl.UseSendSideBWE {
		subscriberConfig.RTPHeaderExtension.Video = append(subscriberConfig.RTPHeaderExtension.Video, sdp.TransportCCURI)
		subscriberConfig.RTCPFeedback.Video = append(subscriberConfig.RTCPFeedback.Video, webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC})
	} else {
		subscriberConfig.RTPHeaderExtension.Video = append(subscriberConfig.RTPHeaderExtension.Video, sdp.ABSSendTimeURI)
		subscriberConfig.RTCPFeedback.Video = append(subscriberConfig.RTCPFeedback.Video, webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBGoogREMB})
	}

	if rtcConf.UseICELite {
		s.SetLite(true)
	}

	if len(rtcConf.Interfaces.Includes) != 0 || len(rtcConf.Interfaces.Excludes) != 0 {
		includes := rtcConf.Interfaces.Includes
		excludes := rtcConf.Interfaces.Excludes
		s.SetInterfaceFilter(func(s string) bool {
			// filter by include interfaces
			if len(includes) > 0 {
				for _, iface := range includes {
					if iface == s {
						return true
					}
				}
				return false
			}

			// filter by exclude interfaces
			if len(excludes) > 0 {
				for _, iface := range excludes {
					if iface == s {
						return false
					}
				}
			}
			return true
		})
	}

	return &WebRTCConfig{
		Configuration: c,
		SettingEngine: s,
		Receiver: ReceiverConfig{
			PacketBufferSize: rtcConf.PacketBufferSize,
			maxBitrate:       rtcConf.MaxBitrate,
		},
		UDPMux:         udpMux,
		UDPMuxConn:     udpMuxConn,
		TCPMuxListener: tcpListener,
		Publisher:      publisherConfig,
		Subscriber:     subscriberConfig,
	}, nil
}

func (c *WebRTCConfig) SetBufferFactory(factory *buffer.Factory) {
	c.BufferFactory = factory
	c.SettingEngine.BufferFactory = factory.GetOrNew
}
