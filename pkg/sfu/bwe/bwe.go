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
	"fmt"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/pion/rtcp"
)

// ------------------------------------------------

const (
	DefaultRTT         = float64(0.070) // 70 ms
	RTTSmoothingFactor = float64(0.5)
)

// ------------------------------------------------

type BWEType int

const (
	BWETypeNone BWEType = iota
	BWETypeRemote
	BWETypeSendSide
)

func (b BWEType) String() string {
	switch b {
	case BWETypeNone:
		return "NONE"
	case BWETypeRemote:
		return "REMOTE"
	case BWETypeSendSide:
		return "SEND_SIDE"
	default:
		return fmt.Sprintf("%d", int(b))
	}
}

// ------------------------------------------------

type CongestionState int

const (
	CongestionStateNone CongestionState = iota
	CongestionStateEarlyWarning
	CongestionStateCongested
)

func (c CongestionState) String() string {
	switch c {
	case CongestionStateNone:
		return "NONE"
	case CongestionStateEarlyWarning:
		return "EARLY_WARNING"
	case CongestionStateCongested:
		return "CONGESTED"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

// ------------------------------------------------

type BWE interface {
	Type() BWEType

	SetBWEListener(bweListner BWEListener)

	Reset()

	HandleREMB(
		receivedEstimate int64,
		expectedBandwidthUsage int64,
		sentPackets uint32,
		repeatedNacks uint32,
	)

	// TWCC sequence number
	RecordPacketSendAndGetSequenceNumber(
		atMicro int64,
		size int,
		isRTX bool,
		probeClusterId ccutils.ProbeClusterId,
		isProbe bool,
	) uint16

	HandleTWCCFeedback(report *rtcp.TransportLayerCC)

	UpdateRTT(rtt float64)

	CongestionState() CongestionState

	CanProbe() bool
	ProbeDuration() time.Duration
	ProbeClusterStarting(pci ccutils.ProbeClusterInfo)
	ProbeClusterDone(pci ccutils.ProbeClusterInfo)
	ProbeClusterIsGoalReached() bool
	ProbeClusterFinalize() (ccutils.ProbeSignal, int64, bool)
}

// ------------------------------------------------

type BWEListener interface {
	OnCongestionStateChange(fromState CongestionState, toState CongestionState, estimatedAvailableChannelCapacity int64)
}

// ------------------------------------------------
