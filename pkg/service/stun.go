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
	"net"
	"strconv"

	"github.com/pion/turn/v2"
	"github.com/pkg/errors"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/pionlogger"

	"github.com/livekit/livekit-server/pkg/config"
)

func NewStunServer(conf *config.Config) (*turn.Server, error) {
	if conf.STUN.UDPPort <= 0 {
		return nil, errors.New("invalid STUN port")
	}

	serverConfig := turn.ServerConfig{
		LoggerFactory: pionlogger.NewLoggerFactory(logger.GetLogger()),
	}

	var logValues []interface{}
	udpListener, err := net.ListenPacket("udp4", "0.0.0.0:"+strconv.Itoa(conf.STUN.UDPPort))
	if err != nil {
		return nil, errors.Wrap(err, "could not listen on STUN UDP port")
	}

	packetConfig := turn.PacketConnConfig{
		PacketConn: udpListener,
	}
	serverConfig.PacketConnConfigs = append(serverConfig.PacketConnConfigs, packetConfig)
	logValues = append(logValues, "stun.UDPPort", conf.STUN.UDPPort)

	logger.Infow("Starting STUN server", logValues...)
	return turn.NewServer(serverConfig)
}
