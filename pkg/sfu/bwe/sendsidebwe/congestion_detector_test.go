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

package sendsidebwe

import (
	"testing"

	"github.com/pion/rtcp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
)

// the reserved symbol has no receive delta, so its packet is neither acked nor lost,
// and the next received packet uses the next delta
func TestHandleTWCCFeedbackReservedSymbol(t *testing.T) {
	// small delta, reserved, reserved, small delta, not received
	for name, chunks := range map[string][]rtcp.PacketStatusChunk{
		"status vector": {
			&rtcp.StatusVectorChunk{
				SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
				SymbolList: []uint16{
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedWithoutDelta,
					rtcp.TypeTCCPacketReceivedWithoutDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketNotReceived,
				},
			},
		},
		"run length": {
			&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: 1},
			&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketReceivedWithoutDelta, RunLength: 2},
			&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: 1},
			&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived, RunLength: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCongestionDetector(congestionDetectorParams{
				Config: defaultCongestionDetectorConfig,
				Logger: logger.GetLogger(),
			})
			// the first packet sent is the send time base and never joins a group, so keep it out of the report
			now := mono.UnixMicro()
			c.RecordPacketSendAndGetSequenceNumber(now, 1200, false, ccutils.ProbeClusterIdInvalid, false)
			sns := make([]uint16, 5)
			for i := range sns {
				sns[i] = c.RecordPacketSendAndGetSequenceNumber(now+int64(i+1)*1000, 1200, false, ccutils.ProbeClusterIdInvalid, false)
			}

			fb := &rtcp.TransportLayerCC{
				Header: rtcp.Header{
					Count: rtcp.FormatTCC,
					Type:  rtcp.TypeTransportSpecificFeedback,
				},
				MediaSSRC:          1,
				BaseSequenceNumber: sns[0],
				PacketStatusCount:  uint16(len(sns)),
				ReferenceTime:      100,
				FbPktCount:         1,
				PacketChunks:       chunks,
				RecvDeltas: []*rtcp.RecvDelta{
					{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: 1000},
					{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: 2000},
				},
			}
			fb.Header.Padding = fb.MarshalSize()%4 != 0
			fb.Header.Length = uint16(fb.MarshalSize()/4 - 1)

			// round trip so the report is exactly what a remote peer can deliver
			raw, err := fb.Marshal()
			require.NoError(t, err)
			pkts, err := rtcp.Unmarshal(raw)
			require.NoError(t, err)

			require.NotPanics(t, func() { c.HandleTWCCFeedback(pkts[0].(*rtcp.TransportLayerCC)) })

			// receive time is relative to the first received packet
			require.Equal(t, int64(2000), c.packetTracker.getPacketInfoExisting(sns[3]).recvTime)

			acked, lost := 0, 0
			for _, pg := range c.packetGroups {
				acked += pg.acked.numPackets()
				lost += pg.lost.numPackets()
			}
			require.Equal(t, 2, acked)
			require.Equal(t, 1, lost)
		})
	}
}
