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

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

var _ bwe.BWE = (*SendSideBWE)(nil)

//
// Based on a simplified/modified version of JitterPath paper
// (https://homepage.iis.sinica.edu.tw/papers/lcs/2114-F.pdf)
//
// TWCC feedback is uesed to calcualte delta one-way-delay.
// It is accumulated/propagated to determine in which region
// groups of packets are operating in.
//
// In simplified terms,
//   o JQR (Join Queuing Region) is when channel is congested.
//   o DQR (Disjoint Queuing Region) is when channel is not.
//
// Packets are grouped and thresholds applied to smooth over
// small variations. For example, in the paper,
//    if propagated_queuing_delay + delta_one_way_delay > 0 {
//       possibly_operating_in_jqr
//    }
// But, in this implementation it is checked at packet group level,
// i. e. using queuing delay and aggreated delta one-way-delay of
// the group and a minimum value threshold is applied before declaring
// that a group is in JQR.
//
// There is also hysteresis to make transisitons smoother, i.e. if the
// metric is above a certain threshold, it is JQR and it is DQR only if it
// is below a certain value and the gap in between those two thresholds
// are treated as interdeterminate groups.
//

// ---------------------------------------------------------------------------

type SendSideBWEConfig struct {
	CongestionDetector CongestionDetectorConfig `yaml:"congestion_detector,omitempty"`
}

var (
	DefaultSendSideBWEConfig = SendSideBWEConfig{
		CongestionDetector: defaultCongestionDetectorConfig,
	}
)

// ---------------------------------------------------------------------------

type SendSideBWEParams struct {
	Config SendSideBWEConfig
	Logger logger.Logger
}

type SendSideBWE struct {
	bwe.NullBWE

	params SendSideBWEParams

	*congestionDetector
}

func NewSendSideBWE(params SendSideBWEParams) *SendSideBWE {
	return &SendSideBWE{
		params: params,
		congestionDetector: newCongestionDetector(congestionDetectorParams{
			Config: params.Config.CongestionDetector,
			Logger: params.Logger,
		}),
	}
}

func (r *SendSideBWE) Type() bwe.BWEType {
	return bwe.BWETypeSendSide
}

func (s *SendSideBWE) SetBWEListener(bweListener bwe.BWEListener) {
	s.congestionDetector.SetBWEListener(bweListener)
}

func (s *SendSideBWE) Reset() {
	s.congestionDetector.Reset()
}

func (s *SendSideBWE) RecordPacketSendAndGetSequenceNumber(
	atMicro int64,
	size int,
	isRTX bool,
	probeClusterId ccutils.ProbeClusterId,
	isProbe bool,
) uint16 {
	return s.congestionDetector.RecordPacketSendAndGetSequenceNumber(atMicro, size, isRTX, probeClusterId, isProbe)
}

func (s *SendSideBWE) HandleTWCCFeedback(report *rtcp.TransportLayerCC) {
	s.congestionDetector.HandleTWCCFeedback(report)
}

func (s *SendSideBWE) UpdateRTT(rtt float64) {
	s.congestionDetector.UpdateRTT(rtt)
}

func (s *SendSideBWE) CongestionState() bwe.CongestionState {
	return s.congestionDetector.CongestionState()
}

func (s *SendSideBWE) CanProbe() bool {
	return s.congestionDetector.CanProbe()
}

func (s *SendSideBWE) ProbeDuration() time.Duration {
	return s.congestionDetector.ProbeDuration()
}

func (s *SendSideBWE) ProbeClusterStarting(pci ccutils.ProbeClusterInfo) {
	s.congestionDetector.ProbeClusterStarting(pci)
}

func (s *SendSideBWE) ProbeClusterDone(pci ccutils.ProbeClusterInfo) {
	s.congestionDetector.ProbeClusterDone(pci)
}

func (s *SendSideBWE) ProbeClusterIsGoalReached() bool {
	return s.congestionDetector.ProbeClusterIsGoalReached()
}

func (s *SendSideBWE) ProbeClusterFinalize() (ccutils.ProbeSignal, int64, bool) {
	return s.congestionDetector.ProbeClusterFinalize()
}

// ------------------------------------------------
