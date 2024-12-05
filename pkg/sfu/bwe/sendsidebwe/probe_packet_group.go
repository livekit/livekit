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

package sendsidebwe

import (
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"go.uber.org/zap/zapcore"
)

// -------------------------------------------------------------

var (
	// large numbers to treat a probe packet group as one
	DefaultPacketGroupConfigProbe = PacketGroupConfig{
		MinPackets:        16384,
		MaxWindowDuration: time.Minute,
	}
)

// -------------------------------------------------------------

type probePacketGroupParams struct {
	ProbeClusterInfo ccutils.ProbeClusterInfo
	Config           PacketGroupConfig
	WeightedLoss     WeightedLossConfig
	Logger           logger.Logger
}

type probePacketGroup struct {
	params probePacketGroupParams
	*packetGroup
}

func newProbePacketGroup(params probePacketGroupParams) *probePacketGroup {
	return &probePacketGroup{
		params: params,
		packetGroup: newPacketGroup(packetGroupParams{
			Config:       params.Config,
			WeightedLoss: params.WeightedLoss,
			Logger:       params.Logger,
		}, 0),
	}
}

func (p *probePacketGroup) Add(pi *packetInfo, sendDelta, recvDelta int64, isLost bool) error {
	if pi.probeClusterId != p.params.ProbeClusterInfo.Id {
		return nil
	}

	return p.packetGroup.Add(pi, sendDelta, recvDelta, isLost)
}

func (p *probePacketGroup) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if p == nil {
		return nil
	}

	e.AddObject("probeClusterInfo", p.params.ProbeClusterInfo)
	e.AddObject("packetGroup", p.packetGroup)
	return nil
}
