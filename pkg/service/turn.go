package service

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"

	"github.com/pion/turn/v2"
	"github.com/pkg/errors"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	logging "github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const (
	LivekitRealm = "livekit"

	allocateRetries = 50
	turnMinPort     = 1024
	turnMaxPort     = 30000
)

func NewTurnServer(conf *config.Config, authHandler turn.AuthHandler, standalone bool) (*turn.Server, error) {
	turnConf := conf.TURN
	if !turnConf.Enabled {
		return nil, nil
	}

	if turnConf.TLSPort <= 0 && turnConf.UDPPort <= 0 {
		return nil, errors.New("invalid TURN ports")
	}

	serverConfig := turn.ServerConfig{
		Realm:         LivekitRealm,
		AuthHandler:   authHandler,
		LoggerFactory: logging.NewLoggerFactory(logger.GetLogger()),
	}
	var relayAddrGen turn.RelayAddressGenerator = &turn.RelayAddressGeneratorPortRange{
		RelayAddress: net.ParseIP(conf.RTC.NodeIP),
		Address:      "0.0.0.0",
		MinPort:      turnConf.RelayPortRangeStart,
		MaxPort:      turnConf.RelayPortRangeEnd,
		MaxRetries:   allocateRetries,
	}
	if standalone {
		relayAddrGen = telemetry.NewRelayAddressGenerator(relayAddrGen)
	}
	var logValues []interface{}

	logValues = append(logValues, "turn.relay_range_start", turnConf.RelayPortRangeStart)
	logValues = append(logValues, "turn.relay_range_end", turnConf.RelayPortRangeEnd)

	if turnConf.TLSPort > 0 {
		if turnConf.Domain == "" {
			return nil, errors.New("TURN domain required")
		}

		if !IsValidDomain(turnConf.Domain) {
			return nil, errors.New("TURN domain is not correct")
		}

		if !turnConf.ExternalTLS {
			cert, err := tls.LoadX509KeyPair(turnConf.CertFile, turnConf.KeyFile)
			if err != nil {
				return nil, errors.Wrap(err, "TURN tls cert required")
			}

			tlsListener, err := tls.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(turnConf.TLSPort),
				&tls.Config{
					MinVersion:   tls.VersionTLS12,
					Certificates: []tls.Certificate{cert},
				})
			if err != nil {
				return nil, errors.Wrap(err, "could not listen on TURN TCP port")
			}
			if standalone {
				tlsListener = telemetry.NewListener(tlsListener)
			}

			listenerConfig := turn.ListenerConfig{
				Listener:              tlsListener,
				RelayAddressGenerator: relayAddrGen,
			}
			serverConfig.ListenerConfigs = append(serverConfig.ListenerConfigs, listenerConfig)
		} else {
			tcpListener, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(turnConf.TLSPort))
			if err != nil {
				return nil, errors.Wrap(err, "could not listen on TURN TCP port")
			}
			if standalone {
				tcpListener = telemetry.NewListener(tcpListener)
			}

			listenerConfig := turn.ListenerConfig{
				Listener:              tcpListener,
				RelayAddressGenerator: relayAddrGen,
			}
			serverConfig.ListenerConfigs = append(serverConfig.ListenerConfigs, listenerConfig)
		}
		logValues = append(logValues, "turn.portTLS", turnConf.TLSPort, "turn.externalTLS", turnConf.ExternalTLS)
	}

	if turnConf.UDPPort > 0 {
		udpListener, err := net.ListenPacket("udp4", "0.0.0.0:"+strconv.Itoa(turnConf.UDPPort))
		if err != nil {
			return nil, errors.Wrap(err, "could not listen on TURN UDP port")
		}

		if standalone {
			udpListener = telemetry.NewPacketConn(udpListener, prometheus.Incoming)
		}

		packetConfig := turn.PacketConnConfig{
			PacketConn:            udpListener,
			RelayAddressGenerator: relayAddrGen,
		}
		serverConfig.PacketConnConfigs = append(serverConfig.PacketConnConfigs, packetConfig)
		logValues = append(logValues, "turn.portUDP", turnConf.UDPPort)
	}

	logger.Infow("Starting TURN server", logValues...)
	return turn.NewServer(serverConfig)
}

func newTurnAuthHandler(roomStore ObjectStore) turn.AuthHandler {
	return func(username, realm string, srcAddr net.Addr) (key []byte, ok bool) {
		// room id should be the username, create a hashed room id
		rm, _, err := roomStore.LoadRoom(context.Background(), livekit.RoomName(username), false)
		if err != nil {
			return nil, false
		}

		return turn.GenerateAuthKey(username, LivekitRealm, rm.TurnPassword), true
	}
}
