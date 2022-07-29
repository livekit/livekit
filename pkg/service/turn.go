package service

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"

	"github.com/pion/turn/v2"
	"github.com/pkg/errors"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	logging "github.com/livekit/livekit-server/pkg/logger"
)

const (
	LivekitRealm = "livekit"

	allocateRetries = 50
	turnMinPort     = 1024
	turnMaxPort     = 30000
)

func NewTurnServer(conf *config.Config, authHandler turn.AuthHandler) (*turn.Server, error) {
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
	relayAddrGen := &turn.RelayAddressGeneratorPortRange{
		RelayAddress: net.ParseIP(conf.RTC.NodeIP),
		Address:      "0.0.0.0",
		MinPort:      turnConf.RelayPortRangeStart,
		MaxPort:      turnConf.RelayPortRangeEnd,
		MaxRetries:   allocateRetries,
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
			if len(conf.Autocert.Dtls) == 0 {
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
			} else {
				tlsListener, err := tlsListener("0.0.0.0:"+strconv.Itoa(turnConf.TLSPort), conf.Autocert.Dtls)
				if err != nil {
					return nil, errors.Wrap(err, "could not run autocert on TURN TCP port")
				}
				listenerConfig := turn.ListenerConfig{
					Listener:              tlsListener,
					RelayAddressGenerator: relayAddrGen,
				}
				serverConfig.ListenerConfigs = append(serverConfig.ListenerConfigs, listenerConfig)
			}
		} else {
			tcpListener, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(turnConf.TLSPort))
			if err != nil {
				return nil, errors.Wrap(err, "could not listen on TURN TCP port")
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
		rm, err := roomStore.LoadRoom(context.Background(), livekit.RoomName(username))
		if err != nil {
			return nil, false
		}

		return turn.GenerateAuthKey(username, LivekitRealm, rm.TurnPassword), true
	}
}

func tlsListener(addr, hosts string) (lnTls net.Listener, err error) {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(hosts),
		Cache:      autocert.DirCache("/tmp/certs"),
	}

	cfg := &tls.Config{
		GetCertificate: m.GetCertificate,
		NextProtos: []string{
			"http/1.1", acme.ALPNProto,
		},
	}

	// Let's Encrypt tls-alpn-01 only works on port 443.
	ln, err := net.Listen("tcp4", addr) /* #nosec G102 */
	if err != nil {
		return
	}

	lnTls = tls.NewListener(ln, cfg)
	return
}
