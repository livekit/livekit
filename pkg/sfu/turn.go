package sfu

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pion/dtls/v2"
	"github.com/pion/logging"
	"github.com/pion/turn/v2"
)

const (
	turnMinPort = 32768
	turnMaxPort = 46883
	sfuMinPort  = 46884
	sfuMaxPort  = 60999
)

type TurnAuth struct {
	Credentials string `mapstructure:"credentials"`
	Secret      string `mapstructure:"secret"`
}

// WebRTCConfig defines parameters for ice
type TurnConfig struct {
	Enabled   bool     `mapstructure:"enabled"`
	Realm     string   `mapstructure:"realm"`
	Address   string   `mapstructure:"address"`
	Cert      string   `mapstructure:"cert"`
	Key       string   `mapstructure:"key"`
	Auth      TurnAuth `mapstructure:"auth"`
	PortRange []uint16 `mapstructure:"portrange"`
}

func InitTurnServer(conf TurnConfig, auth func(username, realm string, srcAddr net.Addr) ([]byte, bool)) (*turn.Server, error) {
	var listeners []turn.ListenerConfig

	// Create a UDP listener to pass into pion/turn
	// pion/turn itself doesn't allocate any UDP sockets, but lets the user pass them in
	// this allows us to add logging, storage or modify inbound/outbound traffic
	udpListener, err := net.ListenPacket("udp4", conf.Address)
	if err != nil {
		return nil, err
	}
	// Create a TCP listener to pass into pion/turn
	// pion/turn itself doesn't allocate any TCP listeners, but lets the user pass them in
	// this allows us to add logging, storage or modify inbound/outbound traffic
	tcpListener, err := net.Listen("tcp4", conf.Address)
	if err != nil {
		return nil, err
	}

	addr := strings.Split(conf.Address, ":")

	var minPort uint16 = turnMinPort
	var maxPort uint16 = turnMaxPort

	if len(conf.PortRange) == 2 {
		minPort = conf.PortRange[0]
		maxPort = conf.PortRange[1]
	}

	if len(conf.Cert) > 0 && len(conf.Key) > 0 {
		// Create a TLS listener to pass into pion/turn
		cert, err := tls.LoadX509KeyPair(conf.Cert, conf.Key)
		if err != nil {
			return nil, err
		}
		config := tls.Config{Certificates: []tls.Certificate{cert}}
		config.Rand = rand.Reader
		tlsListener := tls.NewListener(tcpListener, &config)
		listeners = append(listeners, turn.ListenerConfig{
			Listener: tlsListener,
			RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
				RelayAddress: net.ParseIP(addr[0]),
				Address:      "0.0.0.0",
				MinPort:      minPort,
				MaxPort:      maxPort,
			},
		})
		// Create a DTLS listener to pass into pion/turn
		ctx := context.Background()
		dtlsConf := &dtls.Config{
			Certificates:         []tls.Certificate{cert},
			ExtendedMasterSecret: dtls.RequireExtendedMasterSecret,
			// Create timeout context for accepted connection.
			ConnectContextMaker: func() (context.Context, func()) {
				return context.WithTimeout(ctx, 30*time.Second)
			},
		}
		port, err := strconv.ParseInt(addr[1], 10, 64)
		if err != nil {
			return nil, err
		}
		a := &net.UDPAddr{IP: net.ParseIP(addr[0]), Port: int(port)}
		dtlsListener, err := dtls.Listen("udp4", a, dtlsConf)
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, turn.ListenerConfig{
			Listener: dtlsListener,
			RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
				RelayAddress: net.ParseIP(addr[0]),
				Address:      "0.0.0.0",
				MinPort:      minPort,
				MaxPort:      maxPort,
			},
		})
	}

	if auth == nil {
		if conf.Auth.Secret != "" {
			logger := logging.NewDefaultLeveledLoggerForScope("lt-creds", logging.LogLevelTrace, os.Stdout)
			auth = turn.NewLongTermAuthHandler(conf.Auth.Secret, logger)
		} else {
			usersMap := map[string][]byte{}
			for _, kv := range regexp.MustCompile(`(\w+)=(\w+)`).FindAllStringSubmatch(conf.Auth.Credentials, -1) {
				usersMap[kv[1]] = turn.GenerateAuthKey(kv[1], conf.Realm, kv[2])
			}
			if len(usersMap) == 0 {
				Logger.Error(fmt.Errorf("No turn auth provided"), "Got err")
			}
			auth = func(username string, realm string, srcAddr net.Addr) ([]byte, bool) {
				if key, ok := usersMap[username]; ok {
					return key, true
				}
				return nil, false
			}
		}
	}

	return turn.NewServer(turn.ServerConfig{
		Realm: conf.Realm,
		// Set AuthHandler callback
		// This is called everytime a user tries to authenticate with the TURN server
		// Return the key for that user, or false when no user is found
		AuthHandler: auth,
		// ListenerConfig is a list of Listeners and the Configuration around them
		ListenerConfigs: append(listeners, turn.ListenerConfig{
			Listener: tcpListener,
			RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
				RelayAddress: net.ParseIP(addr[0]),
				Address:      "0.0.0.0",
				MinPort:      minPort,
				MaxPort:      maxPort,
			},
		},
		),
		// PacketConnConfigs is a list of UDP Listeners and the Configuration around them
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: udpListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: net.ParseIP(addr[0]),
					Address:      "0.0.0.0",
					MinPort:      minPort,
					MaxPort:      maxPort,
				},
			},
		},
	})
}
