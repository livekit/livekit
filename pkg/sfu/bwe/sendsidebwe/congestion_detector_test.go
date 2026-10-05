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

// pion parses the reserved symbol without producing a receive delta for it
func TestHandleTWCCFeedbackReservedSymbol(t *testing.T) {
	for name, chunk := range map[string]rtcp.PacketStatusChunk{
		"status vector": &rtcp.StatusVectorChunk{
			SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
			SymbolList: []uint16{
				rtcp.TypeTCCPacketReceivedSmallDelta,
				rtcp.TypeTCCPacketReceivedWithoutDelta,
				rtcp.TypeTCCPacketReceivedWithoutDelta,
				rtcp.TypeTCCPacketReceivedWithoutDelta,
				rtcp.TypeTCCPacketNotReceived,
				rtcp.TypeTCCPacketNotReceived,
				rtcp.TypeTCCPacketNotReceived,
			},
		},
		"run length": &rtcp.RunLengthChunk{
			PacketStatusSymbol: rtcp.TypeTCCPacketReceivedWithoutDelta,
			RunLength:          4,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCongestionDetector(congestionDetectorParams{
				Config: defaultCongestionDetectorConfig,
				Logger: logger.GetLogger(),
			})
			now := mono.UnixMicro()
			base := c.RecordPacketSendAndGetSequenceNumber(now, 1200, false, ccutils.ProbeClusterIdInvalid, false)
			for i := int64(1); i < 4; i++ {
				c.RecordPacketSendAndGetSequenceNumber(now+i*1000, 1200, false, ccutils.ProbeClusterIdInvalid, false)
			}

			fb := &rtcp.TransportLayerCC{
				Header: rtcp.Header{
					Count: rtcp.FormatTCC,
					Type:  rtcp.TypeTransportSpecificFeedback,
				},
				MediaSSRC:          1,
				BaseSequenceNumber: base,
				PacketStatusCount:  4,
				ReferenceTime:      100,
				FbPktCount:         1,
				PacketChunks:       []rtcp.PacketStatusChunk{chunk},
			}
			if _, ok := chunk.(*rtcp.StatusVectorChunk); ok {
				fb.RecvDeltas = []*rtcp.RecvDelta{{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: 1000}}
			}
			fb.Header.Padding = fb.MarshalSize()%4 != 0
			fb.Header.Length = uint16(fb.MarshalSize()/4 - 1)

			// round trip so the report is exactly what a remote peer can deliver
			raw, err := fb.Marshal()
			require.NoError(t, err)
			pkts, err := rtcp.Unmarshal(raw)
			require.NoError(t, err)

			require.NotPanics(t, func() { c.HandleTWCCFeedback(pkts[0].(*rtcp.TransportLayerCC)) })
		})
	}
}
