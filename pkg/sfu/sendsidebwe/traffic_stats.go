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

import "math"

type trafficStats struct {
	minSendTime  int64
	duration     int64
	queuingDelay int64
	sendDelta    int64
	recvDelta    int64
	ackedPackets int
	ackedBytes   int
	lostPackets  int
}

func newTrafficStats() *trafficStats {
	return &trafficStats{}
}

func (ts *trafficStats) Merge(rhs trafficStats) {
	if rhs.minSendTime == 0 || rhs.minSendTime < ts.minSendTime {
		ts.minSendTime = rhs.minSendTime
	}
	ts.duration += rhs.duration
	ts.queuingDelay += rhs.queuingDelay
	ts.sendDelta += rhs.sendDelta
	ts.recvDelta += rhs.recvDelta
	ts.ackedPackets += rhs.ackedPackets
	ts.ackedBytes += rhs.ackedBytes
	ts.lostPackets += rhs.lostPackets
}

func (ts *trafficStats) Duration() int64 {
	return ts.duration
}

func (ts *trafficStats) PropagatedQueuingDelay() int64 {
	return ts.queuingDelay + ts.sendDelta - ts.recvDelta
}

func (ts *trafficStats) AcknowledgedBitrate(minPacketsForLossValidity int, lossPenaltyFactor float64) int64 {
	ackedBitrate := int64(ts.ackedBytes) * 8 * 1e6 / ts.duration
	return int64(float64(ackedBitrate) * ts.CapturedTrafficRatio(minPacketsForLossValidity, lossPenaltyFactor))
}

func (ts *trafficStats) CapturedTrafficRatio(minPacketsForLossValidity int, lossPenaltyFactor float64) float64 {
	if ts.recvDelta == 0 {
		return 0.0
	}

	// apply a penalty for lost packets,
	// tha rationale being packet dropping is a strategy to relieve congestion
	// and if they were not dropped, they would have increased queuing delay,
	// as it is not possible to know the reason for the losses,
	// apply a small penalty to receive delta aggregate to simulate those packets
	// building up queuing delay.
	return min(1.0, float64(ts.sendDelta)/float64(ts.recvDelta+ts.lossPenalty(minPacketsForLossValidity, lossPenaltyFactor)))
}

func (ts *trafficStats) WeightedLoss(minPacketsForLossValidity int, lossPenaltyFactor float64) float64 {
	totalPackets := float64(ts.lostPackets + ts.ackedPackets)
	if int(totalPackets) < minPacketsForLossValidity {
		return 0.0
	}

	lossRatio := float64(0.0)
	if totalPackets != 0 {
		lossRatio = float64(ts.lostPackets) / totalPackets
	}

	pps := totalPackets * 1e6 / float64(ts.duration)

	// Log10 is used to give higher weight for the same loss ratio at higher packet rates,
	// for e.g. with a penalty factor of 0.25
	//    - 10% loss at 20 pps = 0.1 * log10(20) * 0.25 = 0.032
	//    - 10% loss at 100 pps = 0.1 * log10(100) * 0.25 = 0.05
	//    - 10% loss at 1000 pps = 0.1 * log10(1000) * 0.25 = 0.075
	return lossRatio * math.Log10(pps) * lossPenaltyFactor
}

func (ts *trafficStats) lossPenalty(minPacketsForLossValidity int, lossPenaltyFactor float64) int64 {
	return int64(float64(ts.recvDelta) * ts.WeightedLoss(minPacketsForLossValidity, lossPenaltyFactor))
}
