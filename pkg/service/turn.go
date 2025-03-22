// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/jxskiss/base62"
	"github.com/pion/turn/v4"
	"github.com/pkg/errors"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/pionlogger"

	"github.com/livekit/livekit-server/pkg/config"
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
		LoggerFactory: pionlogger.NewLoggerFactory(logger.GetLogger()),
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

func getTURNAuthHandlerFunc(handler *TURNAuthHandler) turn.AuthHandler {
	return handler.HandleAuth
}

type TURNAuthHandler struct {
	keyProvider auth.KeyProvider
}

func NewTURNAuthHandler(keyProvider auth.KeyProvider) *TURNAuthHandler {
	return &TURNAuthHandler{
		keyProvider: keyProvider,
	}
}

func (h *TURNAuthHandler) CreateUsername(apiKey string, pID livekit.ParticipantID) string {
	return base62.EncodeToString([]byte(fmt.Sprintf("%s|%s", apiKey, pID)))
}

func (h *TURNAuthHandler) ParseUsername(username string) (apiKey string, pID livekit.ParticipantID, err error) {
	decoded, err := base62.DecodeString(username)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 2 {
		return "", "", errors.New("invalid username")
	}

	return parts[0], livekit.ParticipantID(parts[1]), nil
}

func (h *TURNAuthHandler) CreatePassword(apiKey string, pID livekit.ParticipantID) (string, error) {
	secret := h.keyProvider.GetSecret(apiKey)
	if secret == "" {
		return "", ErrInvalidAPIKey
	}
	keyInput := fmt.Sprintf("%s|%s", secret, pID)
	sum := sha256.Sum256([]byte(keyInput))
	return base62.EncodeToString(sum[:]), nil
}

func (h *TURNAuthHandler) HandleAuth(username, realm string, srcAddr net.Addr) (key []byte, ok bool) {
	decoded, err := base62.DecodeString(username)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 2 {
		return nil, false
	}
	password, err := h.CreatePassword(parts[0], livekit.ParticipantID(parts[1]))
	if err != nil {
		logger.Warnw("could not create TURN password", err, "username", username)
		return nil, false
	}
	return turn.GenerateAuthKey(username, LivekitRealm, password), true
}
