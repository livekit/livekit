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
	"github.com/livekit/protocol/utils/mono"
	"go.uber.org/zap/zapcore"
)

// -------------------------------------------------------------

type ProbePacketGroupConfig struct {
	PacketGroup PacketGroupConfig `yaml:"packet_group,omitempty"`

	SettleWaitNumRTT uint32        `yaml:"settle_wait_num_rtt,omitempty"`
	SettleWaitMin    time.Duration `yaml:"settle_wait_min,omitempty"`
	SettleWaitMax    time.Duration `yaml:"settle_wait_max,omitempty"`
}

var (
	// large numbers to treat a probe packet group as one
	defaultProbePacketGroupConfig = ProbePacketGroupConfig{
		PacketGroup: PacketGroupConfig{
			MinPackets:        16384,
			MaxWindowDuration: time.Minute,
		},

		SettleWaitNumRTT: 5,
		SettleWaitMin:    250 * time.Millisecond,
		SettleWaitMax:    5 * time.Second,
	}
)

// -------------------------------------------------------------

type probePacketGroupParams struct {
	Config       ProbePacketGroupConfig
	WeightedLoss WeightedLossConfig
	Logger       logger.Logger
}

type probePacketGroup struct {
	params probePacketGroupParams
	pci    ccutils.ProbeClusterInfo
	*packetGroup
	maxSequenceNumber uint64
	doneAt            time.Time
}

func newProbePacketGroup(params probePacketGroupParams, pci ccutils.ProbeClusterInfo) *probePacketGroup {
	return &probePacketGroup{
		params: params,
		pci:    pci,
		packetGroup: newPacketGroup(
			packetGroupParams{
				Config:       params.Config.PacketGroup,
				WeightedLoss: params.WeightedLoss,
				Logger:       params.Logger,
			},
			0,
		),
	}
}

func (p *probePacketGroup) ProbeClusterDone(pci ccutils.ProbeClusterInfo) {
	if p.pci.Id != pci.Id {
		return
	}

	p.pci.Result = pci.Result
	p.doneAt = mono.Now()
}

func (p *probePacketGroup) ProbeClusterInfo() ccutils.ProbeClusterInfo {
	return p.pci
}

func (p *probePacketGroup) MaybeFinalizeProbe(maxSequenceNumber uint64, rtt float64) (ccutils.ProbeClusterInfo, bool) {
	if p.doneAt.IsZero() {
		return ccutils.ProbeClusterInfoInvalid, false
	}

	if maxSequenceNumber != 0 && p.maxSequenceNumber >= maxSequenceNumber {
		return p.pci, true
	}

	settleWait := time.Duration(float64(p.params.Config.SettleWaitNumRTT) * rtt * float64(time.Second))
	if settleWait < p.params.Config.SettleWaitMin {
		settleWait = p.params.Config.SettleWaitMin
	}
	if settleWait > p.params.Config.SettleWaitMax {
		settleWait = p.params.Config.SettleWaitMax
	}
	if time.Since(p.doneAt) < settleWait {
		return ccutils.ProbeClusterInfoInvalid, false
	}

	return p.pci, true
}

func (p *probePacketGroup) Add(pi *packetInfo, sendDelta, recvDelta int64, isLost bool) error {
	if pi.probeClusterId != p.pci.Id {
		return nil
	}

	p.maxSequenceNumber = max(p.maxSequenceNumber, pi.sequenceNumber)

	return p.packetGroup.Add(pi, sendDelta, recvDelta, isLost)
}

func (p *probePacketGroup) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if p == nil {
		return nil
	}

	e.AddObject("pci", p.pci)
	e.AddObject("packetGroup", p.packetGroup)
	e.AddUint64("maxSequenceNumber", p.maxSequenceNumber)
	e.AddTime("doneAt", p.doneAt)
	return nil
}
