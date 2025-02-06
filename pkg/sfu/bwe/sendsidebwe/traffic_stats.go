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
	"math"
	"time"

	"github.com/livekit/protocol/logger"
	"go.uber.org/zap/zapcore"
)

// -----------------------------------------------------------

type WeightedLossConfig struct {
	MinDurationForLossValidity time.Duration `yaml:"min_duration_for_loss_validity,omitempty"`
	BaseDuration               time.Duration `yaml:"base_duration,omitempty"`
	BasePPS                    int           `yaml:"base_pps,omitempty"`
	LossPenaltyFactor          float64       `yaml:"loss_penalty_factor,omitempty"`
}

var (
	defaultWeightedLossConfig = WeightedLossConfig{
		MinDurationForLossValidity: 100 * time.Millisecond,
		BaseDuration:               500 * time.Millisecond,
		BasePPS:                    30,
		LossPenaltyFactor:          0.25,
	}
)

// -----------------------------------------------------------

type trafficStatsParams struct {
	Config WeightedLossConfig
	Logger logger.Logger
}

type trafficStats struct {
	params trafficStatsParams

	minSendTime  int64
	maxSendTime  int64
	sendDelta    int64
	recvDelta    int64
	ackedPackets int
	ackedBytes   int
	lostPackets  int
	lostBytes    int
}

func newTrafficStats(params trafficStatsParams) *trafficStats {
	return &trafficStats{
		params: params,
	}
}

func (ts *trafficStats) Merge(rhs *trafficStats) {
	if ts.minSendTime == 0 || rhs.minSendTime < ts.minSendTime {
		ts.minSendTime = rhs.minSendTime
	}
	if rhs.maxSendTime > ts.maxSendTime {
		ts.maxSendTime = rhs.maxSendTime
	}
	ts.sendDelta += rhs.sendDelta
	ts.recvDelta += rhs.recvDelta
	ts.ackedPackets += rhs.ackedPackets
	ts.ackedBytes += rhs.ackedBytes
	ts.lostPackets += rhs.lostPackets
	ts.lostBytes += rhs.lostBytes
}

func (ts *trafficStats) NumBytes() int {
	return ts.ackedBytes + ts.lostBytes
}

func (ts *trafficStats) Duration() int64 {
	return ts.maxSendTime - ts.minSendTime
}

func (ts *trafficStats) AcknowledgedBitrate() int64 {
	duration := ts.Duration()
	if duration == 0 {
		return 0
	}

	ackedBitrate := float64(ts.ackedBytes) * 8 * 1e6 / float64(ts.Duration())
	return int64(ackedBitrate * ts.CapturedTrafficRatio())
}

func (ts *trafficStats) CapturedTrafficRatio() float64 {
	if ts.recvDelta == 0 {
		return 0.0
	}

	// apply a penalty for lost packets,
	// the rationale being packet dropping is a strategy to relieve congestion
	// and if they were not dropped, they would have increased queuing delay,
	// as it is not possible to know the reason for the losses,
	// apply a small penalty to receive delta aggregate to simulate those packets
	// building up queuing delay.
	return min(1.0, float64(ts.sendDelta)/float64(ts.recvDelta+ts.lossPenalty()))
}

func (ts *trafficStats) WeightedLoss() float64 {
	durationMicro := ts.Duration()
	if time.Duration(durationMicro*1000) < ts.params.Config.MinDurationForLossValidity {
		return 0.0
	}

	totalPackets := float64(ts.lostPackets + ts.ackedPackets)
	pps := totalPackets * 1e6 / float64(durationMicro)

	// longer duration, i. e. more time resolution, lower pps is acceptable as the measurement is more stable
	deltaDuration := time.Duration(durationMicro*1000) - ts.params.Config.BaseDuration
	if deltaDuration < 0 {
		deltaDuration = 0
	}
	threshold := math.Exp(-deltaDuration.Seconds()) * float64(ts.params.Config.BasePPS)
	if pps < threshold {
		return 0.0
	}

	lossRatio := float64(0.0)
	if totalPackets != 0 {
		lossRatio = float64(ts.lostPackets) / totalPackets
	}

	// Log10 is used to give higher weight for the same loss ratio at higher packet rates,
	// for e.g.
	//    - 10% loss at 20 pps = 0.1 * log10(20) = 0.130
	//    - 10% loss at 100 pps = 0.1 * log10(100) = 0.2
	//    - 10% loss at 1000 pps = 0.1 * log10(1000) = 0.3
	return lossRatio * math.Log10(pps)
}

func (ts *trafficStats) lossPenalty() int64 {
	return int64(float64(ts.recvDelta) * ts.WeightedLoss() * ts.params.Config.LossPenaltyFactor)
}

func (ts *trafficStats) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if ts == nil {
		return nil
	}

	e.AddInt64("minSendTime", ts.minSendTime)
	e.AddInt64("maxSendTime", ts.maxSendTime)
	duration := time.Duration(ts.Duration() * 1000)
	e.AddDuration("duration", duration)

	e.AddInt("ackedPackets", ts.ackedPackets)
	e.AddInt("ackedBytes", ts.ackedBytes)
	e.AddInt("lostPackets", ts.lostPackets)
	e.AddInt("lostBytes", ts.lostBytes)

	bitrate := float64(0)
	if duration != 0 {
		bitrate = float64(ts.ackedBytes*8) / duration.Seconds()
		e.AddFloat64("bitrate", bitrate)
	}

	e.AddInt64("sendDelta", ts.sendDelta)
	e.AddInt64("recvDelta", ts.recvDelta)
	e.AddInt64("groupDelay", ts.recvDelta-ts.sendDelta)

	totalPackets := ts.lostPackets + ts.ackedPackets
	if duration != 0 {
		e.AddFloat64("pps", float64(totalPackets)/duration.Seconds())
	}
	if (totalPackets) != 0 {
		e.AddFloat64("rawLoss", float64(ts.lostPackets)/float64(totalPackets))
	}
	e.AddFloat64("weightedLoss", ts.WeightedLoss())
	e.AddInt64("lossPenalty", ts.lossPenalty())

	capturedTrafficRatio := ts.CapturedTrafficRatio()
	e.AddFloat64("capturedTrafficRatio", capturedTrafficRatio)
	e.AddFloat64("estimatedAvailableChannelCapacity", bitrate*capturedTrafficRatio)
	return nil
}
