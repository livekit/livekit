package service

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	"github.com/pion/turn/v2"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
)

const (
	allocateRetries = 1000
	livekitRealm    = "livekit"
)

func NewTurnServer(conf *config.Config, roomStore RoomStore, node routing.LocalNode) (*turn.Server, error) {
	turnConf := conf.TURN
	if !turnConf.Enabled {
		return nil, nil
	}
	if turnConf.Domain == "" {
		return nil, errors.New("TURN domain required")
	}
	if turnConf.TLSPort == 0 {
		return nil, errors.New("invalid TURN tcp port")
	}

	cert, err := tls.LoadX509KeyPair(turnConf.CertFile, turnConf.KeyFile)
	if err != nil {
		return nil, errors.Wrap(err, "TURN tls cert required")
	}

	tlsListener, err := tls.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(turnConf.TLSPort), &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not listen on TURN TCP port")
	}

	serverConfig := turn.ServerConfig{
		Realm:       livekitRealm,
		AuthHandler: newTurnAuthHandler(roomStore),
		ListenerConfigs: []turn.ListenerConfig{
			{
				Listener: tlsListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: net.ParseIP(node.Ip),
					Address:      "0.0.0.0",
					MinPort:      turnConf.PortRangeStart,
					MaxPort:      turnConf.PortRangeEnd,
					MaxRetries:   allocateRetries,
				},
			},
		},
	}

	logger.Infow("Starting TURN server",
		"TCP port", turnConf.TLSPort,
		"portRange", fmt.Sprintf("%d-%d", turnConf.PortRangeStart, turnConf.PortRangeEnd))
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
