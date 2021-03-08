package service

import (
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
	serverConfig := turn.ServerConfig{
		Realm:       livekitRealm,
		AuthHandler: newTurnAuthHandler(roomStore),
	}

	if turnConf.TCPPort > 0 {
		tcpListener, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(turnConf.TCPPort))
		if err != nil {
			return nil, errors.Wrap(err, "could not listen on TURN TCP port")
		}
		serverConfig.ListenerConfigs = []turn.ListenerConfig{
			{
				Listener: tcpListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: net.ParseIP(node.Ip),
					Address:      "0.0.0.0",
					MinPort:      turnConf.PortRangeStart,
					MaxPort:      turnConf.PortRangeEnd,
					MaxRetries:   allocateRetries,
				},
			},
		}
	}

	if turnConf.UDPPort > 0 {
		udpListener, err := net.ListenPacket("udp4", "0.0.0.0:"+strconv.Itoa(turnConf.UDPPort))
		if err != nil {
			return nil, errors.Wrap(err, "could not listen on TURN UDP port")
		}
		serverConfig.PacketConnConfigs = []turn.PacketConnConfig{
			{
				PacketConn: udpListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: net.ParseIP(node.Ip), // Claim that we are listening on IP passed by user (This should be your Public IP)
					Address:      "0.0.0.0",            // But actually be listening on every interface
					MinPort:      turnConf.PortRangeStart,
					MaxPort:      turnConf.PortRangeEnd,
					MaxRetries:   allocateRetries,
				},
			},
		}
	}

	logger.Infow("Starting TURN server",
		"TCP port", turnConf.TCPPort,
		"UDP port", turnConf.UDPPort,
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
