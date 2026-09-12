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
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/transport/v4/packetio"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/flexfec"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
	"github.com/livekit/livekit-server/pkg/testutils/vnettest"
	"github.com/livekit/mediatransportutil/pkg/codec"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// Capture decrypted RTP at the subscriber before Pion demultiplexes repair SSRCs.
// Retain independent packets because SRTP reuses its input buffer.
type fecWireCapture struct {
	io.ReadWriteCloser
	capture *packetCapture
}

func (c *fecWireCapture) Write(raw []byte) (int, error) {
	p := &rtp.Packet{}
	if err := p.Unmarshal(raw); err == nil {
		c.capture.add(p.Clone())
	}
	return c.ReadWriteCloser.Write(raw)
}

func TestDownTrackFlexFECOnWire(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		enabled, negotiated, encrypted bool
		pacing                         string
		level                          *livekit.FECProtection
		protectionPercent              uint32
	}{
		{"pass through", true, true, false, "pass", livekit.FECProtection_FEC_MEDIUM.Enum(), 25},
		{"queued", true, true, false, "queue", livekit.FECProtection_FEC_MEDIUM.Enum(), 25},
		{"low", true, true, false, "queue", livekit.FECProtection_FEC_LOW.Enum(), 15},
		{"high", true, true, false, "leaky", livekit.FECProtection_FEC_HIGH.Enum(), 35},
		{"none", true, true, false, "pass", livekit.FECProtection_FEC_NONE.Enum(), 0},
		{"default none", true, true, false, "pass", nil, 0},
		{"paced encrypted", true, true, true, "leaky", livekit.FECProtection_FEC_MEDIUM.Enum(), 25},
		{"subscriber declines", true, false, false, "pass", livekit.FECProtection_FEC_HIGH.Enum(), 35},
		{"disabled", false, true, false, "pass", livekit.FECProtection_FEC_HIGH.Enum(), 35},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := vnettest.NewHosts(t)
			factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
			capture := &packetCapture{}
			makePC := func(sender bool, enableFEC bool) *webrtc.PeerConnection {
				me := vnettest.NewMediaEngine(t, vnettest.MediaEngineConfig{Video: true})
				if enableFEC {
					require.NoError(t, me.RegisterCodec(webrtc.RTPCodecParameters{
						RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeFlexFEC03, ClockRate: 90000, SDPFmtpLine: "repair-window=10000000"},
						PayloadType:        118,
					}, webrtc.RTPCodecTypeVideo))
				}
				net := h.AnswerNet
				if sender {
					net = h.OfferNet
				}
				se := vnettest.NewSettingEngine(net)
				if sender {
					se.BufferFactory = factory.GetOrNew
				} else {
					se.BufferFactory = func(kind packetio.BufferPacketType, _ uint32) io.ReadWriteCloser {
						b := packetio.NewBuffer()
						if kind == packetio.RTPBufferPacket {
							return &fecWireCapture{ReadWriteCloser: b, capture: capture}
						}
						return b
					}
				}
				pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se), webrtc.WithInterceptorRegistry(&interceptor.Registry{})).NewPeerConnection(webrtc.Configuration{})
				require.NoError(t, err)
				t.Cleanup(func() { _ = pc.Close() })
				return pc
			}
			sender, sub := makePC(true, tc.enabled), makePC(false, tc.negotiated)
			captureTrack(sub) // drain media normally, even when FEC is declined
			var p pacer.Pacer
			switch tc.pacing {
			case "queue":
				p = pacer.NewNoQueue(logger.GetLogger(), newNullBWE())
			case "leaky":
				p = pacer.NewLeakyBucket(logger.GetLogger(), newNullBWE(), time.Millisecond, 10_000_000)
			default:
				p = pacer.NewPassThrough(logger.GetLogger(), newNullBWE())
			}
			t.Cleanup(p.Stop)
			var repairsSent atomic.Int32
			dt, err := sfu.NewDownTrack(sfu.DownTrackParams{
				Codecs:   []webrtc.RTPCodecParameters{vp8CodecParams},
				Receiver: newFakeTrackReceiver(vp8CodecParams), BufferFactory: factory,
				Pacer: p, Logger: logger.GetLogger(), Listener: &sfufakes.FakeDownTrackListener{},
				StreamID: "fec-stream", SubID: "fec-sub", MaxTrack: 500,
				EnableFlexFEC: tc.enabled, IsEncrypted: tc.encrypted,
				OnFECSent: func(n int, _ int) { repairsSent.Add(int32(n)) },
			})
			require.NoError(t, err)
			if tc.level != nil {
				dt.SetFECProtection(*tc.level)
			}
			tr, err := sender.AddTransceiverFromTrack(dt, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
			require.NoError(t, err)
			dt.SetTransceiver(tr)
			vnettest.SignalPair(t, sender, sub)
			dt.SetConnected()
			require.Eventually(t, dt.IsWritableForTest, 10*time.Second, 10*time.Millisecond)
			dt.ForceForwardLayerForTest(buffer.VideoLayer{Spatial: 0, Temporal: 0})
			t.Cleanup(func() { dt.CloseWithFlush(false, true) })

			negotiated := tc.enabled && tc.negotiated
			fecSSRC := uint32(tr.Sender().GetParameters().Encodings[0].FEC.SSRC)
			if negotiated {
				require.NotZero(t, fecSSRC)
				require.Contains(t, sender.LocalDescription().SDP, fmt.Sprintf("a=ssrc-group:FEC-FR %d %d", dt.SSRC(), fecSSRC))
				require.Contains(t, sub.LocalDescription().SDP, "flexfec-03/90000")
			} else {
				require.Zero(t, fecSSRC)
			}

			const mediaCount = 20
			sendMedia := func(start, count int) {
				for i := start; i < start+count; i++ {
					ep, err := testutils.GetTestExtPacketVP8(&testutils.TestExtPacketParams{
						SequenceNumber: uint16(6000 + i), Timestamp: uint32(270000 + i*3000), SSRC: 0x44444444,
						PayloadType: 96, PayloadSize: 100 + i, IsKeyFrame: true, Marker: true,
					}, &codec.VP8{FirstByte: 0x10, S: true, PictureID: uint16(i + 1), IsKeyFrame: true})
					require.NoError(t, err)
					ep.Packet.Payload = distinctivePayload(byte(i), 100+i)
					require.EqualValues(t, 1, dt.WriteRTP(ep, 0))
					clear(ep.Packet.Payload) // forwarding and encoding must own retained bytes
				}
			}
			sendMedia(0, mediaCount)
			expected := mediaCount
			if negotiated {
				expected += mediaCount * int(tc.protectionPercent) / 100
			}
			require.Eventually(t, func() bool { return capture.count() >= expected }, 10*time.Second, 10*time.Millisecond)
			require.Equal(t, expected, capture.count())
			packets := capture.all()
			var media, repair []*rtp.Packet
			for _, packet := range packets {
				if packet.SSRC == dt.SSRC() {
					media = append(media, packet)
				} else {
					require.Equal(t, fecSSRC, packet.SSRC)
					require.EqualValues(t, 118, packet.PayloadType)
					repair = append(repair, packet)
				}
			}
			require.Len(t, media, mediaCount)
			require.EqualValues(t, len(repair), repairsSent.Load())
			if !negotiated || tc.protectionPercent == 0 {
				require.Empty(t, repair)
				return
			}
			require.Len(t, repair, mediaCount*int(tc.protectionPercent)/100)
			for i := 1; i < len(repair); i++ {
				require.Equal(t, repair[i-1].SequenceNumber+1, repair[i].SequenceNumber)
			}
			// Drop media in the second and third groups (protected by every enabled
			// preset) and recover from the actual wire
			// representation, including translated SSRC/PT/sequence and payload.
			stored := map[uint16][]byte{}
			for i, packet := range media {
				if i != 7 && i != 12 {
					stored[packet.SequenceNumber], _ = packet.Marshal()
				}
			}
			decoder := flexfec.NewDecoder(fecSSRC, dt.SSRC(), func(sn uint16, dst []byte) (int, error) {
				raw, ok := stored[sn]
				if !ok {
					return 0, errors.New("dropped media")
				}
				return copy(dst, raw), nil
			}, logger.GetLogger())
			recoveredPackets := map[uint16][]byte{}
			for _, packet := range repair {
				for _, recovered := range decoder.DecodeFEC(packet) {
					raw, err := recovered.Marshal()
					require.NoError(t, err)
					recoveredPackets[recovered.SequenceNumber] = raw
					stored[recovered.SequenceNumber] = raw
				}
			}
			require.Len(t, recoveredPackets, 2)
			for _, lost := range []*rtp.Packet{media[7], media[12]} {
				want, _ := lost.Marshal()
				require.Equal(t, want, recoveredPackets[lost.SequenceNumber])
			}
			if tc.name == "pass through" {
				// Updating protection keeps the same negotiated repair stream.
				sequence := repair[len(repair)-1].SequenceNumber
				start := mediaCount
				for _, update := range []struct {
					level   livekit.FECProtection
					percent int
				}{
					{livekit.FECProtection_FEC_HIGH, 35},
					{livekit.FECProtection_FEC_NONE, 0},
					{livekit.FECProtection_FEC_LOW, 15},
				} {
					before := capture.count()
					dt.SetFECProtection(update.level)
					sendMedia(start, mediaCount)
					start += mediaCount
					numRepair := mediaCount * update.percent / 100
					require.Eventually(t, func() bool { return capture.count() >= before+mediaCount+numRepair }, 5*time.Second, 10*time.Millisecond)
					require.Equal(t, before+mediaCount+numRepair, capture.count())
					for _, packet := range capture.all()[before:] {
						if packet.SSRC == fecSSRC {
							require.Equal(t, sequence+1, packet.SequenceNumber)
							sequence = packet.SequenceNumber
						}
					}
				}

				// Resume a subscription on its cached transceiver, including the
				// repair sequence. The receiver's existing SRTP replay window must
				// accept every packet without another negotiation.
				dt.CloseWithFlush(false, false)
				state := dt.GetState()
				require.Equal(t, sequence+1, state.FECState.NextSequenceNumber)
				next, err := sfu.NewDownTrack(sfu.DownTrackParams{
					Codecs:        []webrtc.RTPCodecParameters{vp8CodecParams},
					Receiver:      newFakeTrackReceiver(vp8CodecParams),
					BufferFactory: factory,
					Pacer:         p,
					Logger:        logger.GetLogger(),
					Listener:      &sfufakes.FakeDownTrackListener{},
					StreamID:      "fec-stream",
					SubID:         "fec-sub",
					MaxTrack:      500,
					EnableFlexFEC: true,
				})
				require.NoError(t, err)
				next.OnBinding(func(err error) {
					if err == nil {
						next.SeedState(state)
					}
				})
				next.SetTransceiver(tr)
				next.SetFECProtection(livekit.FECProtection_FEC_MEDIUM)
				require.NoError(t, tr.Sender().ReplaceTrack(next))
				dt = next
				dt.SetConnected()
				require.Eventually(t, dt.IsWritableForTest, 5*time.Second, 10*time.Millisecond)
				dt.ForceForwardLayerForTest(buffer.VideoLayer{Spatial: 0, Temporal: 0})
				before := capture.count()
				sendMedia(start, mediaCount)
				require.Eventually(t, func() bool { return capture.count() >= before+25 }, 5*time.Second, 10*time.Millisecond)
				repairs := 0
				for _, packet := range capture.all()[before:] {
					if packet.SSRC == fecSSRC {
						require.Equal(t, sequence+1, packet.SequenceNumber)
						sequence = packet.SequenceNumber
						repairs++
					}
				}
				require.Equal(t, 5, repairs)
			}
		})
	}
}
