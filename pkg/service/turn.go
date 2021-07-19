package service

import (
	"crypto/tls"
	"net"
	"strconv"

	"github.com/pion/turn/v2"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
)

const (
	allocateRetries = 50
	turnMinPort     = 1024
	turnMaxPort     = 30000
	livekitRealm    = "livekit"
)

func NewTurnServer(conf *config.Config, roomStore RoomStore, node routing.LocalNode) (*turn.Server, error) {
	turnConf := conf.TURN
	if !turnConf.Enabled {
		return nil, nil
	}

	if turnConf.TLSPort == 0 && turnConf.UDPPort == 0 {
		return nil, errors.New("invalid TURN ports")
	}

	serverConfig := turn.ServerConfig{
		Realm:         livekitRealm,
		AuthHandler:   newTurnAuthHandler(roomStore),
		LoggerFactory: logger.LoggerFactory(),
	}
	relayAddrGen := &turn.RelayAddressGeneratorPortRange{
		RelayAddress: net.ParseIP(node.Ip),
		Address:      "0.0.0.0",
		MinPort:      turnMinPort,
		MaxPort:      turnMaxPort,
		MaxRetries:   allocateRetries,
	}
	var logValues []interface{}

	if turnConf.TLSPort > 0 {
		if turnConf.Domain == "" {
			return nil, errors.New("TURN domain required")
		}

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

		listenerConfig := turn.ListenerConfig{
			Listener:              tlsListener,
			RelayAddressGenerator: relayAddrGen,
		}
		serverConfig.ListenerConfigs = append(serverConfig.ListenerConfigs, listenerConfig)
		logValues = append(logValues, "turn.portTLS", turnConf.TLSPort)
	}

	if turnConf.UDPPort > 0 {
		udpListener, err := net.ListenPacket("udp4", "0.0.0.0:"+strconv.Itoa(turnConf.UDPPort))
		if err != nil {
			return nil, errors.Wrap(err, "could not listen on TURN UDP port")
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

func newTurnAuthHandler(roomStore RoomStore) turn.AuthHandler {
	return func(username, realm string, srcAddr net.Addr) (key []byte, ok bool) {
		// room id should be the username, create a hashed room id
		rm, err := roomStore.GetRoom(username)
		if err != nil {
			return nil, false
		}

		return turn.GenerateAuthKey(username, livekitRealm, rm.TurnPassword), true
	}
}
