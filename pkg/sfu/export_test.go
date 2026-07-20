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

package sfu

import "github.com/livekit/livekit-server/pkg/sfu/buffer"

// Test-only seams that expose DownTrack internals to the external sfu_test package.
// This file is only compiled into the test binary (its name ends in _test.go), so it
// does not affect the production API. The external test package is required because the
// integration tests import the streamallocator package, which itself imports sfu (an
// import cycle that an in-package test cannot resolve).

func (d *DownTrack) IsWritableForTest() bool { return d.writable.Load() }

func (d *DownTrack) PayloadTypeRTXForTest() uint32 { return d.payloadTypeRTX.Load() }

func (d *DownTrack) RTPStatsActiveForTest() bool { return d.rtpStats.IsActive() }

func (d *DownTrack) LastReceiverReportTimeForTest() int64 {
	return d.rtpStats.LastReceiverReportTime()
}

func (d *DownTrack) ExtIDsForTest() (absSendTime int, transportWide int) {
	return d.absSendTimeExtID, d.transportWideExtID
}

func (d *DownTrack) HandleRTCPForTest(pkt []byte) { d.handleRTCP(pkt) }

func (d *DownTrack) SetRTPHeaderExtensionsForTest() { d.setRTPHeaderExtensions() }

// ForceForwardLayerForTest forces the forwarder onto a valid current/target layer so a
// (non-simulcast) video packet will be forwarded without driving full allocation.
func (d *DownTrack) ForceForwardLayerForTest(layer buffer.VideoLayer) {
	d.forwarder.ForceLayerForTest(layer)
}

// RetransmitForTest drives the (unexported) retransmitPacket path with an in-package
// extPacketMeta built from exported values, so the external test can exercise the
// retransmit path.
func (d *DownTrack) RetransmitForTest(
	sourceSeqNo uint64,
	targetSeqNo uint16,
	timestamp uint32,
	extTimestamp uint64,
	layer int8,
	sourcePkt []byte,
) (int, error) {
	epm := extPacketMeta{
		packetMeta: packetMeta{
			sourceSeqNo: sourceSeqNo,
			targetSeqNo: targetSeqNo,
			timestamp:   timestamp,
			layer:       layer,
		},
		extSequenceNumber: uint64(targetSeqNo),
		extTimestamp:      extTimestamp,
	}
	return d.retransmitPacket(&epm, sourcePkt, false)
}
