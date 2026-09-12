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

package pacer

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/livekit-server/pkg/sfu/flexfec"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type fecTestBWE struct {
	bwe.BWE
	sizes []int
}

func (b *fecTestBWE) RecordPacketSendAndGetSequenceNumber(_ int64, size int, _ bool, _ ccutils.ProbeClusterId, _ bool) uint16 {
	b.sizes = append(b.sizes, size)
	return uint16(len(b.sizes))
}

type fecTestWriter struct {
	packets        []*rtp.Packet
	failMedia      bool
	failRepair     bool
	noWriter       bool
	noRepairWriter bool
}

func (w *fecTestWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	if w.noWriter || (w.noRepairWriter && h.SSRC == 456) {
		// Pion returns (0, nil) until its interceptor writer is installed.
		return 0, nil
	}
	if (w.failMedia && h.SSRC == 123) || (w.failRepair && h.SSRC == 456) {
		return 0, io.ErrClosedPipe
	}
	// Model an interceptor changing a header after the pacer has patched it.
	if h.SSRC == 123 {
		_ = h.SetExtension(7, []byte{byte(h.SequenceNumber), 9})
	}
	p := (&rtp.Packet{Header: *h, Payload: payload}).Clone()
	w.packets = append(w.packets, p)
	return p.MarshalSize(), nil
}
func (w *fecTestWriter) Write([]byte) (int, error) { panic("unexpected raw write") }

func TestPacerFECFinalHeadersAndAccounting(t *testing.T) {
	bw := &fecTestBWE{}
	b := NewBase(logger.GetLogger(), bw)
	w := &fecTestWriter{}
	sent, payloadBytes := 0, 0
	encoder := flexfec.NewEncoder(115, 456, func(n int, bytes int) { sent += n; payloadBytes += bytes })
	encoder.SetProtectionPercent(20)
	total := 0
	for i := range flexfec.MediaPacketsPerGroup {
		p := PacketFactory.Get().(*Packet)
		h := &rtp.Header{Version: 2, SSRC: 123, PayloadType: 96, SequenceNumber: uint16(i), Timestamp: uint32(i * 3000)}
		_ = h.SetExtension(3, []byte{0, 0, 0})
		_ = h.SetExtension(5, []byte{0, 0})
		_ = h.SetExtension(7, []byte{0, 0})
		*p = Packet{Header: h, HeaderSize: h.MarshalSize(), Payload: []byte{byte(i), 4, 5}, WriteStream: w, FEC: encoder, AbsSendTimeExtID: 3, TransportWideExtID: 5}
		n, err := b.SendPacket(p)
		require.NoError(t, err)
		total += n
	}
	require.Len(t, w.packets, 6)
	require.Len(t, bw.sizes, 6)
	require.Equal(t, 1, sent)
	require.Equal(t, len(w.packets[5].Payload), payloadBytes)
	wireBytes := 0
	media := map[uint16][]byte{}
	for i, p := range w.packets {
		wireBytes += p.MarshalSize()
		require.Equal(t, p.MarshalSize(), bw.sizes[i], "BWE must account for every media and repair byte")
		require.EqualValues(t, i+1, binary.BigEndian.Uint16(p.GetExtension(5)))
		if i < 5 && i != 2 {
			media[p.SequenceNumber], _ = p.Marshal()
		}
	}
	require.Equal(t, wireBytes, total, "repair bytes must consume the pacer budget")
	decoder := flexfec.NewDecoder(456, 123, func(sn uint16, dst []byte) (int, error) {
		raw, ok := media[sn]
		if !ok {
			return 0, errors.New("missing")
		}
		return copy(dst, raw), nil
	}, logger.GetLogger())
	recovered := decoder.DecodeFEC(w.packets[5])
	require.Len(t, recovered, 1)
	expected, _ := w.packets[2].Marshal()
	actual, _ := recovered[0].Marshal()
	require.Equal(t, expected, actual, "FEC must cover post-interceptor headers")
}

func TestPacerFECSkipsRTXProbesAndFailedWrites(t *testing.T) {
	for _, mode := range []string{"rtx", "probe", "media failure", "repair failure", "writer not ready", "repair writer not ready"} {
		t.Run(mode, func(t *testing.T) {
			b := NewBase(logger.GetLogger(), nil)
			w := &fecTestWriter{failMedia: mode == "media failure", failRepair: mode == "repair failure", noWriter: mode == "writer not ready", noRepairWriter: mode == "repair writer not ready"}
			sent := 0
			e := flexfec.NewEncoder(115, 456, func(n int, _ int) { sent += n })
			e.SetProtectionPercent(20)
			for i := range 10 {
				p := PacketFactory.Get().(*Packet)
				*p = Packet{Header: &rtp.Header{Version: 2, SSRC: 123, SequenceNumber: uint16(i)}, HeaderSize: 12, Payload: []byte{1}, FEC: e, WriteStream: w, IsRTX: mode == "rtx", IsProbe: mode == "probe"}
				n, err := b.SendPacket(p)
				if w.failMedia {
					require.ErrorIs(t, err, io.ErrClosedPipe)
					require.Zero(t, n)
				} else {
					require.NoError(t, err)
					if w.noWriter {
						require.Zero(t, n)
					} else {
						require.Positive(t, n)
					}
				}
			}
			require.Zero(t, sent)
			for _, p := range w.packets {
				require.EqualValues(t, 123, p.SSRC)
			}
		})
	}
}
