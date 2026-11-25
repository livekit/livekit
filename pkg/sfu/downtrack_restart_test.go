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

package sfu_test

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	. "github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
	"github.com/livekit/livekit-server/pkg/testutils/vnettest"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func TestDownTrackRetransmitAcrossReceiverRestart(t *testing.T) {
	testDownTrackRepairAcrossReceiverRestart(t, false)
}

func TestDownTrackProbeAcrossReceiverRestart(t *testing.T) {
	testDownTrackRepairAcrossReceiverRestart(t, true)
}

func testDownTrackRepairAcrossReceiverRestart(t *testing.T, probe bool) {
	t.Helper()
	for _, stage := range []string{"none", "before lookup", "before read", "after read"} {
		t.Run(stage, func(t *testing.T) {
			h := vnettest.NewHosts(t)
			factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
			cp := &capturingPacer{inner: pacer.NewPassThrough(logger.GetLogger(), newNullBWE())}
			t.Cleanup(cp.Stop)
			dh := newBoundDownTrack(t, h, factory, vp8CodecParams, cp, vnettest.MediaEngineConfig{Video: true, HeaderExtensions: probe})
			require.NotZero(t, dh.dt.SSRCRTX())
			if probe {
				listener := &sfufakes.FakeDownTrackStreamAllocatorListener{}
				listener.IsBWEEnabledReturns(true)
				listener.BWETypeReturns(bwe.BWETypeRemote)
				dh.dt.SetStreamAllocatorListener(listener)
				dh.dt.SetRTPHeaderExtensionsForTest()
				abs, twcc := dh.dt.ExtIDsForTest()
				require.NotZero(t, abs|twcc)
			}
			dh.dt.ForceForwardLayerForTest(buffer.VideoLayer{Spatial: 0, Temporal: 0})

			receiver := NewReceiverBase(ReceiverBaseParams{
				TrackID: "source", StreamID: "stream", Kind: webrtc.RTPCodecTypeVideo,
				Codec: vp8CodecParams, Logger: logger.GetLogger(),
			}, &livekit.TrackInfo{Sid: "source", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA}, ReceiverCodecStateNormal)
			t.Cleanup(func() { receiver.Close("test", true) })
			buff := buffer.NewBuffer(123, 500, 500)
			buff.OnRtcpFeedback(func([]rtcp.Packet) {})
			require.NoError(t, buff.Bind(webrtc.RTPParameters{Codecs: []webrtc.RTPCodecParameters{vp8CodecParams}}, vp8CodecParams.RTPCodecCapability, 0))
			receiver.AddBuffer(buff, 0)
			require.NoError(t, receiver.AddDownTrack(dh.dt))

			write := func(tail byte) {
				t.Helper()
				pkt := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 6000, Timestamp: 90000, SSRC: 123, Marker: true}, Payload: []byte{0x10, 0x00, tail}}
				raw, err := pkt.Marshal()
				require.NoError(t, err)
				_, err = buff.Write(raw)
				require.NoError(t, err)
			}
			write(0x11)
			ep, err := buff.ReadExtended(make([]byte, 1500))
			require.NoError(t, err)
			require.Equal(t, int32(1), dh.dt.WriteRTP(ep, 0))
			original := append([]byte(nil), ep.Packet.Payload...)
			buffer.ReleaseExtPacket(ep)
			if probe {
				feedReceiverReport(t, dh.dt, 6000)
			}

			restart := func() {
				receiver.Restart("test")
				write(0x22)
			}
			dh.receiver.ReadRTPCalls(func(buf []byte, layer uint8, esn uint64, streamGeneration uint64) (int, error) {
				if stage == "before read" {
					restart()
				}
				n, err := receiver.ReadRTP(buf, layer, esn, streamGeneration)
				if stage == "after read" {
					restart()
				}
				return n, err
			})
			// Age the original past the sequencer's duplicate-NACK suppression.
			time.Sleep(NACKIgnoreWindowForTest + 10*time.Millisecond)
			if stage == "before lookup" {
				restart()
			}
			var repairs []capturedRTX
			if probe {
				require.Positive(t, dh.dt.WriteProbePackets(1, false))
				for _, p := range cp.probePackets() {
					if len(p.payload) != 0 {
						repairs = append(repairs, p)
					}
				}
			} else {
				dh.dt.RetransmitPacketsForTest([]uint16{6000})
				repairs = cp.rtxPackets()
			}
			if stage == "none" || stage == "after read" {
				require.Len(t, repairs, 1)
			}
			for _, repair := range repairs {
				if len(repair.payload) < 2 || binary.BigEndian.Uint16(repair.payload[:2]) != 6000 || repair.hdr.Timestamp != 90000 || !bytes.Equal(repair.payload[2:], original) {
					t.Errorf("repair combined old metadata with different media: timestamp=%d payload=%x original=%x", repair.hdr.Timestamp, repair.payload, original)
				}
			}
		})
	}
}
