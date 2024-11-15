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

package bwe

import "github.com/pion/rtcp"

type NullBWE struct {
}

func (n *NullBWE) SetBWEListener(_bweListener BWEListener) {}

func (n *NullBWE) Reset() {}

func (n *NullBWE) Stop() {}

func (n *NullBWE) HandleREMB(
	_receivedEstimate int64,
	_isInProbe bool,
	_isProbeFinalizing bool,
	_expectedBandwidthUsage int64,
	_sentPackets uint32,
	_repeatedNacks uint32,
) {
}

func (n *NullBWE) HandleTWCCFeedback(_report *rtcp.TransportLayerCC) {}

func (n *NullBWE) ProbingStart() {}

func (n *NullBWE) ProbingEnd() {}

func (n *NullBWE) GetProbeStatus() (isValidSignal bool, isCongesting bool, lowestEstimate int64, highestEstimate int64) {
	return false, false, 0, 0
}

// ------------------------------------------------
