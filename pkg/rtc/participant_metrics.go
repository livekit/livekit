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

package rtc

import (
	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"google.golang.org/protobuf/proto"
)

type ParticipantMetricsParams struct {
	CanSubscribeMetrics bool
	TransportManager    *TransportManager
	Logger              logger.Logger
}

type ParticipantMetrics struct {
	params ParticipantMetricsParams

	closed core.Fuse
}

func NewParticipantMetrics(params ParticipantMetricsParams) *ParticipantMetrics {
	return &ParticipantMetrics{
		params: params,
	}
}

func (p *ParticipantMetrics) Close() {
	p.closed.Break()
}

func (p *ParticipantMetrics) HandleMetrics(metrics *livekit.MetricsBatch) error {
	if p.closed.IsBroken() || !p.params.CanSubscribeMetrics {
		return nil
	}

	// METRICS-TODO:  This is just forwarding. May need some time stamp munging.
	dpData, err := proto.Marshal(&livekit.DataPacket{
		Value: &livekit.DataPacket_Metrics{
			Metrics: metrics,
		},
	})
	if err != nil {
		p.params.Logger.Errorw("failed to marshal data packet", err)
		return err
	}

	return p.params.TransportManager.SendDataPacket(livekit.DataPacket_RELIABLE, dpData)
}
