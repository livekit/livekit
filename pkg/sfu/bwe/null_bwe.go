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

import (
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/pion/rtcp"
)

type NullBWE struct {
}

func (n *NullBWE) SetBWEListener(_bweListener BWEListener) {}

func (n *NullBWE) Reset() {}

func (n *NullBWE) RecordPacketSendAndGetSequenceNumber(
	_atMicro int64,
	_size int,
	_isRTX bool,
	_probeClusterId ccutils.ProbeClusterId,
	_isProbe bool,
) uint16 {
	return 0
}

func (n *NullBWE) HandleREMB(
	_receivedEstimate int64,
	_expectedBandwidthUsage int64,
	_sentPackets uint32,
	_repeatedNacks uint32,
) {
}

func (n *NullBWE) HandleTWCCFeedback(_report *rtcp.TransportLayerCC) {}

func (n *NullBWE) UpdateRTT(rtt float64) {}

func (n *NullBWE) CongestionState() CongestionState {
	return CongestionStateNone
}

func (n *NullBWE) CanProbe() bool {
	return false
}

func (n *NullBWE) ProbeDuration() time.Duration {
	return 0
}

func (n *NullBWE) ProbeClusterStarting(_pci ccutils.ProbeClusterInfo) {}

func (n *NullBWE) ProbeClusterDone(_pci ccutils.ProbeClusterInfo) {}

func (n *NullBWE) ProbeClusterIsGoalReached() bool {
	return false
}

func (n *NullBWE) ProbeClusterFinalize() (ccutils.ProbeSignal, int64, bool) {
	return ccutils.ProbeSignalInconclusive, 0, false
}

// ------------------------------------------------
