// Copyright 2026 LiveKit, Inc.
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

package flexfec

// Minimum number of expected packets a loss sample covers (kLimitNumPackets in
// libwebrtc's SendSideBandwidthEstimation).
const minLossSamplePackets = 20

// Larger jumps of the extended highest sequence number are treated as a
// stream reset rather than as packets sent.
const maxLossSampleSequenceJump = 1 << 15

// LossAggregator turns RTCP reception reports into loss samples for adaptive
// protection, like libwebrtc's SendSideBandwidthEstimation::UpdatePacketsLost.
//
// A receiver can send a report every few packets, for example with each NACK,
// and the fraction lost of such a report is very noisy: one lost packet out of
// four reads as 25%. Fed straight into the max-window loss filter, those
// samples overestimate the loss several times. The aggregator instead derives
// the expected and lost packets from the cumulative counters and produces a
// sample only once it covers at least minLossSamplePackets expected packets.
//
// It is not safe for concurrent use.
type LossAggregator struct {
	initialized bool
	highestSeq  uint32
	totalLost   uint32
	expected    int64
	lost        int64
}

// Update accounts a reception report, given its extended highest sequence
// number and its cumulative number of packets lost. It returns the Q8 loss
// fraction of the packets expected since the previous sample, and false while
// too few packets are expected to produce one.
func (a *LossAggregator) Update(highestSeq uint32, totalLost uint32) (uint8, bool) {
	if !a.initialized {
		a.reset(highestSeq, totalLost)
		return 0, false
	}

	expected := int64(int32(highestSeq - a.highestSeq))
	if expected < 0 || expected > maxLossSampleSequenceJump {
		a.reset(highestSeq, totalLost)
		return 0, false
	}
	// The cumulative number of packets lost is a signed 24-bit value.
	lost := int64(int32((totalLost-a.totalLost)<<8) >> 8)
	a.highestSeq = highestSeq
	a.totalLost = totalLost

	a.expected += expected
	// Duplicates can make the cumulative loss decrease.
	a.lost = max(a.lost+lost, 0)
	if a.expected < minLossSamplePackets {
		return 0, false
	}

	fraction := min(min(a.lost, a.expected)<<8/a.expected, 255)
	a.expected = 0
	a.lost = 0
	return uint8(fraction), true
}

func (a *LossAggregator) reset(highestSeq uint32, totalLost uint32) {
	*a = LossAggregator{initialized: true, highestSeq: highestSeq, totalLost: totalLost}
}
