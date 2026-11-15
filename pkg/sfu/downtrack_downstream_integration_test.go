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

// This file contains real-pion integration tests for the DownTrack downstream
// packet push path.
//
// Rather than mocking the pacer/write stream, these tests wire a real DownTrack
// (optionally with a real StreamAllocator + Pacer + BWE) to a real pion
// PeerConnection and push packets over an in-memory pion vnet to a second real
// pion PeerConnection, which reads them back off the wire.
//
// The suite drives the four ways a DownTrack emits packets downstream and
// verifies the on-wire RTP header and payload produced by each: SSRC, payload
// type, sequence number, timestamp, marker, payload bytes, and the padding
// bit / padding size.
//
//	WriteRTP          - forwarding translated media
//	retransmitPacket  - retransmitting (RTX) media
//	WritePaddingRTP   - padding-only packets
//	WriteProbePackets - RTX probe packets
//
// It also checks that a track paused by congestion resumes once probing finds
// the bandwidth again.
//
// pion's rtp.Packet.Unmarshal rejects a packet whose padding bit is set but whose
// trailing padding-size byte is invalid/zero (errInvalidRTPPadding), so a
// malformed packet is dropped by the receiver and never arrives. Arrival on the
// far side is therefore itself evidence that the packet was serialized correctly.

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/mediatransportutil/pkg/codec"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	. "github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/bwe/remotebwe"
	"github.com/livekit/livekit-server/pkg/sfu/bwe/sendsidebwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/sfu/packettrailer"
	act "github.com/livekit/livekit-server/pkg/sfu/rtpextension/abscapturetime"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
	"github.com/livekit/livekit-server/pkg/sfu/streamallocator"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/livekit-server/pkg/testutils/vnettest"
)

// TestMain sets the global logger once, before any test starts a background
// goroutine that reads it (prometheus.Init starts an hwstats CPU monitor that
// logs via the global logger). Calling logger.SetLogger from inside a test while
// that goroutine runs is a data race on the logger package globals.
func TestMain(m *testing.M) {
	logger.InitFromConfig(&logger.Config{Level: "debug"}, "sfu-test")
	os.Exit(m.Run())
}

// -----------------------------------------------------------------------------
// packet capture on the far side
// -----------------------------------------------------------------------------

type packetCapture struct {
	lock    sync.Mutex
	packets []*rtp.Packet
}

func (c *packetCapture) add(p *rtp.Packet) {
	c.lock.Lock()
	defer c.lock.Unlock()
	cp := *p
	c.packets = append(c.packets, &cp)
}

func (c *packetCapture) count() int {
	c.lock.Lock()
	defer c.lock.Unlock()
	return len(c.packets)
}

func (c *packetCapture) all() []*rtp.Packet {
	c.lock.Lock()
	defer c.lock.Unlock()
	return append([]*rtp.Packet(nil), c.packets...)
}

// captureTrack wires an OnTrack handler on the receiving PC that reads RTP packets
// into the capture until the track ends.
func captureTrack(pc *webrtc.PeerConnection) *packetCapture {
	cap := &packetCapture{}
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				cap.add(pkt)
			}
		}()
	})
	return cap
}

// -----------------------------------------------------------------------------
// fakes (from pkg/sfu/sfufakes) + a null BWE
// -----------------------------------------------------------------------------

// newFakeTrackReceiver returns a counterfeiter FakeTrackReceiver configured just
// enough for a DownTrack to bind and forward: it reports the given codec and fires
// its readiness callback immediately so Bind completes.
func newFakeTrackReceiver(codecParams webrtc.RTPCodecParameters) *sfufakes.FakeTrackReceiver {
	r := &sfufakes.FakeTrackReceiver{}
	r.TrackIDReturns("fake-track")
	r.StreamIDReturns("fake-stream")
	r.CodecReturns(codecParams)
	r.MimeReturns(mime.NormalizeMimeType(codecParams.MimeType))
	r.VideoLayerModeReturns(livekit.VideoLayer_MODE_UNUSED)
	r.HeaderExtensionsReturns(nil)
	r.IsClosedReturns(false)
	r.TrackInfoReturns(&livekit.TrackInfo{})
	r.CodecStateReturns(ReceiverCodecStateNormal)
	r.GetPrimaryReceiverForRedReturns(r)
	r.GetRedReceiverReturns(r)
	r.ReadRTPReturns(0, io.EOF)
	// AddOnReady must invoke its callback immediately so Bind reaches the bound state.
	r.AddOnReadyCalls(func(f func()) { f() })
	return r
}

// nullBWE is a complete no-op bwe.BWE for tests that do not exercise estimation.
// bwe.NullBWE is meant to be embedded and lacks a couple of interface methods.
type nullBWE struct {
	*bwe.NullBWE
}

func newNullBWE() *nullBWE        { return &nullBWE{NullBWE: &bwe.NullBWE{}} }
func (nullBWE) Type() bwe.BWEType { return bwe.BWETypeNone }

// -----------------------------------------------------------------------------
// DownTrack harness
// -----------------------------------------------------------------------------

var (
	opusCodecParams = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		PayloadType:        111,
	}
	vp8CodecParams = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: vnettest.VideoRTCPFeedback()},
		PayloadType:        96,
	}
)

type downTrackHarness struct {
	dt       *DownTrack
	receiver *sfufakes.FakeTrackReceiver
	pacer    pacer.Pacer
	capture  *packetCapture
	sender   *webrtc.PeerConnection
	sub      *webrtc.PeerConnection
}

// newBoundDownTrack builds a real DownTrack, attaches it to a real sender PC,
// negotiates with a real subscriber PC over vnet, and makes it writable.
func newBoundDownTrack(t *testing.T, h *vnettest.Hosts, factory *buffer.Factory, codecParams webrtc.RTPCodecParameters, p pacer.Pacer, meCfg vnettest.MediaEngineConfig, rcvOpts ...func(*sfufakes.FakeTrackReceiver)) *downTrackHarness {
	t.Helper()

	sender := vnettest.NewPeerConnection(t, vnettest.PCConfig{Net: h.OfferNet, MediaEngine: meCfg, BufferFactory: factory.GetOrNew})
	sub := vnettest.NewPeerConnection(t, vnettest.PCConfig{Net: h.AnswerNet, MediaEngine: meCfg, BufferFactory: factory.GetOrNew})
	capture := captureTrack(sub)

	rcv := newFakeTrackReceiver(codecParams)
	for _, opt := range rcvOpts {
		opt(rcv)
	}
	dt, err := NewDownTrack(DownTrackParams{
		Codecs:        []webrtc.RTPCodecParameters{codecParams},
		Receiver:      rcv,
		BufferFactory: factory,
		Pacer:         p,
		Logger:        logger.GetLogger(),
		Listener:      &sfufakes.FakeDownTrackListener{},
		StreamID:      "fake-stream",
		SubID:         "fake-sub",
		MaxTrack:      500,
	})
	require.NoError(t, err)

	tr, err := sender.AddTransceiverFromTrack(dt, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	require.NoError(t, err)
	dt.SetTransceiver(tr)

	vnettest.SignalPair(t, sender, sub)
	dt.SetConnected()

	require.Eventually(t, func() bool {
		return dt.IsWritableForTest()
	}, 10*time.Second, 20*time.Millisecond, "downtrack did not become writable")

	return &downTrackHarness{
		dt:       dt,
		receiver: rcv,
		pacer:    p,
		capture:  capture,
		sender:   sender,
		sub:      sub,
	}
}

// distinctivePayload builds a per-packet payload with recognizable bytes so payload
// fidelity can be asserted end-to-end.
func distinctivePayload(seed byte, n int) []byte {
	b := make([]byte, n)
	for j := range b {
		b[j] = seed + byte(j)
	}
	return b
}

// -----------------------------------------------------------------------------
// spike: confirm vnet + real pion media forwarding works with this pion version
// -----------------------------------------------------------------------------

func TestPionVNetForwardingSpike(t *testing.T) {
	h := vnettest.NewHosts(t)

	sender := vnettest.NewPeerConnection(t, vnettest.PCConfig{Net: h.OfferNet, MediaEngine: vnettest.MediaEngineConfig{Video: true}})
	receiver := vnettest.NewPeerConnection(t, vnettest.PCConfig{Net: h.AnswerNet, MediaEngine: vnettest.MediaEngineConfig{Video: true}})

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "pion-spike",
	)
	require.NoError(t, err)
	_, err = sender.AddTrack(track)
	require.NoError(t, err)

	cap := captureTrack(receiver)

	vnettest.SignalPair(t, sender, receiver)

	// pump a few RTP packets from the sender track
	go func() {
		sn := uint16(1000)
		ts := uint32(0)
		for i := 0; i < 200; i++ {
			_ = track.WriteRTP(&rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: sn,
					Timestamp:      ts,
					SSRC:           0x1234,
				},
				Payload: []byte{0x10, 0x00, 0x00, 0x01, 0x02, 0x03},
			})
			sn++
			ts += 3000
			time.Sleep(2 * time.Millisecond)
		}
	}()

	require.Eventually(t, func() bool {
		return cap.count() >= 5
	}, 10*time.Second, 20*time.Millisecond, "expected to receive forwarded RTP over vnet")
}

// -----------------------------------------------------------------------------
// WriteRTP: media forwarding
// -----------------------------------------------------------------------------

// TestDownTrackForwardsMedia exercises the WriteRTP media forwarding path and
// verifies the full forwarded header/payload on the far side: SSRC, payload type,
// contiguous sequence numbers, timestamps, payload bytes, and that the padding bit
// is cleared regardless of the source packet's padding bit.
func TestDownTrackForwardsMedia(t *testing.T) {
	h := vnettest.NewHosts(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	p := pacer.NewPassThrough(logger.GetLogger(), newNullBWE())
	t.Cleanup(p.Stop)

	dh := newBoundDownTrack(t, h, factory, opusCodecParams, p, vnettest.MediaEngineConfig{})

	const numPackets = 20
	sn := uint16(23333)
	ts := uint32(0xabcdef)
	wantPayload := map[uint16][]byte{}
	wantTS := map[uint16]uint32{}
	for i := 0; i < numPackets; i++ {
		ep, err := testutils.GetTestExtPacket(&testutils.TestExtPacketParams{
			SequenceNumber: sn,
			Timestamp:      ts,
			SSRC:           0x12345678,
			PayloadType:    111,
			PayloadSize:    40,
		})
		require.NoError(t, err)
		// distinctive payload so payload fidelity can be checked on the far side
		payload := distinctivePayload(byte(i+1), 40)
		ep.Packet.Payload = payload
		// every other source packet carries the padding bit; the forwarded packet
		// must not inherit it.
		if i%2 == 0 {
			ep.Packet.Header.Padding = true
		}
		wantPayload[sn] = payload
		wantTS[sn] = ts

		require.Equal(t, int32(1), dh.dt.WriteRTP(ep, 0), "packet %d should be forwarded", i)
		sn++
		ts += 960
	}

	require.Eventually(t, func() bool {
		return dh.capture.count() >= numPackets
	}, 10*time.Second, 20*time.Millisecond, "expected all forwarded media packets to arrive")

	pkts := dh.capture.all()
	seen := map[uint16]bool{}
	for _, pkt := range pkts {
		require.EqualValues(t, dh.dt.SSRC(), pkt.SSRC, "forwarded packet on wrong SSRC")
		require.EqualValues(t, 111, pkt.PayloadType, "forwarded packet has wrong payload type")
		require.False(t, pkt.Header.Padding, "forwarded media packet must not carry padding bit (sn=%d)", pkt.SequenceNumber)

		wp, ok := wantPayload[pkt.SequenceNumber]
		require.True(t, ok, "received unexpected sequence number %d", pkt.SequenceNumber)
		require.Equal(t, wp, pkt.Payload, "forwarded payload mismatch (sn=%d)", pkt.SequenceNumber)
		require.Equal(t, wantTS[pkt.SequenceNumber], pkt.Timestamp, "forwarded timestamp mismatch (sn=%d)", pkt.SequenceNumber)
		seen[pkt.SequenceNumber] = true
	}

	// every source packet must have been forwarded, and the forwarded sequence
	// numbers must be contiguous.
	for want := uint16(23333); want < 23333+numPackets; want++ {
		require.True(t, seen[want], "sequence number %d was not forwarded", want)
	}
}

type discardWriteStream struct{}

func (discardWriteStream) WriteRTP(*rtp.Header, []byte) (int, error) { return 0, nil }
func (discardWriteStream) Write([]byte) (int, error)                 { return 0, nil }

type discardPacer struct {
	*pacer.PassThrough
}

func (d discardPacer) Enqueue(p *pacer.Packet) {
	p.WriteStream = discardWriteStream{}
	d.PassThrough.Enqueue(p)
}

func TestDownTrackWriteRTPNoAlloc(t *testing.T) {
	h := vnettest.NewHosts(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	p := discardPacer{pacer.NewPassThrough(logger.GetLogger(), newNullBWE())}
	t.Cleanup(p.Stop)

	dh := newBoundDownTrack(t, h, factory, opusCodecParams, p, vnettest.MediaEngineConfig{})
	dh.dt.SetAbsCaptureTimeExtIDForTest(5)

	ep, err := testutils.GetTestExtPacket(&testutils.TestExtPacketParams{
		SequenceNumber: 1000,
		Timestamp:      5000,
		SSRC:           0x12345678,
		PayloadType:    111,
		PayloadSize:    40,
	})
	require.NoError(t, err)
	ep.AbsCaptureTimeExt = act.AbsCaptureTimeFromValue(0x0102030405060708, 9)

	const runs = 1000
	forwarded := int32(0)
	allocs := testing.AllocsPerRun(runs, func() {
		ep.Packet.SequenceNumber++
		ep.ExtSequenceNumber++
		ep.Packet.Timestamp += 960
		ep.ExtTimestamp += 960
		forwarded += dh.dt.WriteRTP(ep, 0)
	})
	require.EqualValues(t, runs+1, forwarded, "every write, including the AllocsPerRun warm up, should forward")
	if !utils.RaceEnabled {
		require.Zero(t, allocs, "allocations per WriteRTP")
	}
}

// -----------------------------------------------------------------------------
// retransmitPacket: RTX / retransmission
// -----------------------------------------------------------------------------

// TestDownTrackRetransmitsPacketsAsIs exercises the retransmit path when no RTX
// stream is negotiated, so the packet is re-sent as-is on the primary SSRC/payload
// type. It verifies the retransmitted header/payload on the far side: SSRC, payload
// type, sequence number, timestamp, payload bytes, and that the padding bit is
// cleared regardless of the source packet's padding bit.
func TestDownTrackRetransmitsPacketsAsIs(t *testing.T) {
	h := vnettest.NewHosts(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	p := pacer.NewPassThrough(logger.GetLogger(), newNullBWE())
	t.Cleanup(p.Stop)

	dh := newBoundDownTrack(t, h, factory, opusCodecParams, p, vnettest.MediaEngineConfig{})

	const numPackets = 10
	targetSN := uint16(40000)
	wantPayload := map[uint16][]byte{}
	wantTS := map[uint16]uint32{}
	for i := 0; i < numPackets; i++ {
		// build a source packet that carries the padding bit (valid padding trailer)
		// and a distinctive payload.
		payload := distinctivePayload(byte(i+100), 30)
		src := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        true,
				PaddingSize:    20,
				PayloadType:    111,
				SequenceNumber: uint16(1000 + i),
				Timestamp:      uint32(90000 + i*960),
				SSRC:           0x22222222,
			},
			Payload: payload,
		}
		raw, err := src.Marshal()
		require.NoError(t, err)

		ts := uint32(500000 + i*960)
		wantPayload[targetSN] = payload
		wantTS[targetSN] = ts

		_, err = dh.dt.RetransmitForTest(
			uint64(src.SequenceNumber),
			targetSN,
			ts,
			uint64(ts),
			0,
			raw,
			0,
		)
		require.NoError(t, err)
		targetSN++
	}

	require.Eventually(t, func() bool {
		return dh.capture.count() >= numPackets
	}, 10*time.Second, 20*time.Millisecond, "expected all retransmitted packets to arrive")

	seen := map[uint16]bool{}
	for _, pkt := range dh.capture.all() {
		require.EqualValues(t, dh.dt.SSRC(), pkt.SSRC, "retransmitted packet on wrong SSRC")
		require.EqualValues(t, 111, pkt.PayloadType, "retransmitted packet has wrong payload type")
		require.False(t, pkt.Header.Padding, "retransmitted packet must not carry padding bit (sn=%d)", pkt.SequenceNumber)

		wp, ok := wantPayload[pkt.SequenceNumber]
		require.True(t, ok, "received unexpected sequence number %d", pkt.SequenceNumber)
		require.Equal(t, wp, pkt.Payload, "retransmitted payload mismatch (sn=%d)", pkt.SequenceNumber)
		require.Equal(t, wantTS[pkt.SequenceNumber], pkt.Timestamp, "retransmitted timestamp mismatch (sn=%d)", pkt.SequenceNumber)
		seen[pkt.SequenceNumber] = true
	}

	for want := uint16(40000); want < 40000+numPackets; want++ {
		require.True(t, seen[want], "retransmitted sequence number %d did not arrive", want)
	}
}

// TestDownTrackCapsRetransmissions forwards media, NACKs all of it, and checks that an
// uncapped down track retransmits everything while a capped one retransmits only what its
// budget allows, newest first, without reading the capped packets from the buffer. Capped
// packets NACKed again keep failing while the budget is empty, past maxAck, and are
// retransmitted once media refills it. Repeat NACKs are spaced past the sequencer's
// duplicate window.
func TestDownTrackCapsRetransmissions(t *testing.T) {
	const numPackets = 20

	setup := func(t *testing.T) (*downTrackHarness, *capturingPacer, []byte) {
		h := vnettest.NewHosts(t)
		factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
		cp := &capturingPacer{inner: pacer.NewPassThrough(logger.GetLogger(), newNullBWE())}
		t.Cleanup(cp.Stop)

		// every NACKed packet is read back from the receiver as this source packet
		source, err := (&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 111, SSRC: 0x44444444},
			Payload: distinctivePayload(7, 600),
		}).Marshal()
		require.NoError(t, err)
		dh := newBoundDownTrack(t, h, factory, opusCodecParams, cp, vnettest.MediaEngineConfig{}, func(r *sfufakes.FakeTrackReceiver) {
			r.ReadRTPCalls(func(buf []byte, _ uint8, _ uint64) (int, error) {
				return copy(buf, source), nil
			})
		})
		return dh, cp, source
	}

	// extended, so the sequence number can wrap
	nextSN := uint64(30000)
	nextTS := uint32(1000)
	writeMedia := func(t *testing.T, dh *downTrackHarness, payloadSize int) []uint16 {
		var sns []uint16
		for i := 0; i < numPackets; i++ {
			nextTS += 960
			ep, err := testutils.GetTestExtPacket(&testutils.TestExtPacketParams{
				SequenceNumber: uint16(nextSN),
				SNCycles:       int(nextSN >> 16),
				Timestamp:      nextTS,
				SSRC:           0x44444444,
				PayloadType:    111,
				PayloadSize:    payloadSize,
			})
			require.NoError(t, err)
			require.Equal(t, int32(1), dh.dt.WriteRTP(ep, 0), "packet %d should be forwarded", nextSN)
			sns = append(sns, uint16(nextSN))
			nextSN++
		}
		return sns
	}
	sendNACK := func(t *testing.T, dh *downTrackHarness, sns []uint16) {
		nack := rtcp.TransportLayerNack{MediaSSRC: dh.dt.SSRC()}
		for _, sn := range sns {
			nack.Nacks = append(nack.Nacks, rtcp.NackPair{PacketID: sn})
		}
		b, err := nack.Marshal()
		require.NoError(t, err)
		dh.dt.HandleRTCPForTest(b)
	}
	rtxSeqs := func(cp *capturingPacer) []uint16 {
		var sns []uint16
		for _, p := range cp.rtxPackets() {
			sns = append(sns, p.hdr.SequenceNumber)
		}
		return sns
	}

	t.Run("uncapped", func(t *testing.T) {
		dh, cp, _ := setup(t)
		sns := writeMedia(t, dh, 40)
		sendNACK(t, dh, sns)

		require.Eventually(t, func() bool {
			return len(cp.rtxPackets()) == numPackets
		}, 5*time.Second, 20*time.Millisecond, "expected every packet to be retransmitted")
		require.Equal(t, sns, rtxSeqs(cp), "without a budget the order is unchanged")
	})

	t.Run("capped", func(t *testing.T) {
		dh, cp, source := setup(t)
		// two and a half retransmissions: the third overdraws the budget, a fourth is refused
		dh.dt.SetRTXBudgetForTest(RTXBudgetLimit{MaxRatio: 0.5, BurstBytes: 5 * len(source) / 2})

		sns := writeMedia(t, dh, 40)
		sendNACK(t, dh, sns)

		// the capped count is added after the batch, so the retransmissions are all enqueued by then
		require.Eventually(t, func() bool {
			return dh.dt.RTXCappedForTest() == numPackets-3
		}, 5*time.Second, 20*time.Millisecond, "expected the batch to be capped")
		require.ElementsMatch(t, sns[numPackets-3:], rtxSeqs(cp), "the newest packets go first")
		require.Equal(t, 3, dh.receiver.ReadRTPCallCount(), "a capped retransmission must not read the publisher's buffer")

		// with the budget empty, the capped packets fail again on every NACK, past maxAck
		capped := sns[:numPackets-3]
		for i := 0; i < 4; i++ {
			time.Sleep(NACKIgnoreWindowForTest + 20*time.Millisecond)
			sendNACK(t, dh, capped)
			want := uint64((i + 2) * len(capped))
			require.Eventually(t, func() bool {
				return dh.dt.RTXCappedForTest() == want
			}, 5*time.Second, 20*time.Millisecond, "expected NACK %d to be capped", i+2)
		}

		// media refills the budget, and the earlier capped packets are retransmitted
		writeMedia(t, dh, 600)
		time.Sleep(NACKIgnoreWindowForTest + 20*time.Millisecond)
		sendNACK(t, dh, capped)
		require.Eventually(t, func() bool {
			return len(cp.rtxPackets()) == 6
		}, 5*time.Second, 20*time.Millisecond, "expected capped packets to be retransmitted after the refill")
		require.ElementsMatch(t, append(sns[numPackets-3:], capped[len(capped)-3:]...), rtxSeqs(cp))
	})

	// the budget floor of 1500 bytes sends three 612-byte packets: they must be the three
	// newest of the NACK, whatever its order
	newestOf := func(t *testing.T, start uint64, nackIndexes []int, newestIndexes []int) {
		dh, cp, _ := setup(t)
		dh.dt.SetRTXBudgetForTest(RTXBudgetLimit{MaxRatio: 0.5})

		nextSN = start
		sns := writeMedia(t, dh, 40)
		require.Eventually(t, func() bool {
			return dh.capture.count() >= numPackets
		}, 10*time.Second, 20*time.Millisecond, "expected the media to arrive")
		// past the window the sequencer keeps after a packet is sent
		time.Sleep(NACKIgnoreWindowForTest + 20*time.Millisecond)

		var nacked []uint16
		for _, i := range nackIndexes {
			nacked = append(nacked, sns[i])
		}
		sendNACK(t, dh, nacked)

		var want []uint16
		for _, i := range newestIndexes {
			want = append(want, sns[i])
		}
		require.Eventually(t, func() bool {
			return dh.dt.RTXCappedForTest() == uint64(len(nacked)-len(want))
		}, 5*time.Second, 20*time.Millisecond, "expected all but the newest to be capped")
		require.Equal(t, want, rtxSeqs(cp))
	}

	t.Run("newest first for a NACK in mixed order", func(t *testing.T) {
		newestOf(t, 31000, []int{10, 19, 0, 15, 5}, []int{19, 15, 10})
	})

	t.Run("newest first across the sequence number wrap", func(t *testing.T) {
		// packets 65530..65535 then 0..13: three before the wrap and three after it, out of order
		newestOf(t, 65530, []int{4, 8, 1, 6, 9, 2}, []int{9, 8, 6})
	})
}

// TestDownTrackRetransmitsPacketsViaRTX exercises the retransmit path when an RTX
// stream is negotiated. Each retransmitted packet must be emitted on the RTX SSRC
// with the RTX payload type, carry the original media sequence number as the OSN
// prefix (the first two payload bytes), draw its sequence number from the DownTrack's
// RTX sequence space, and clear the padding bit.
//
// RTX packets are captured at the pacer as the DownTrack builds them: pion's
// receiver-side RTX handling de-RTXes / consumes them, so the RTX-specific header
// fields are not observable on the far side. The packets are still handed to the real
// pacer (and written over pion).
func TestDownTrackRetransmitsPacketsViaRTX(t *testing.T) {
	h := vnettest.NewHosts(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	cp := &capturingPacer{inner: pacer.NewPassThrough(logger.GetLogger(), newNullBWE())}
	t.Cleanup(cp.Stop)

	// VP8 registers an RTX codec, so the bound DownTrack has an RTX SSRC / payload type.
	dh := newBoundDownTrack(t, h, factory, vp8CodecParams, cp, vnettest.MediaEngineConfig{Video: true})
	require.NotZero(t, dh.dt.SSRCRTX(), "RTX ssrc should be negotiated")
	require.NotZero(t, dh.dt.PayloadTypeRTXForTest(), "RTX payload type should be negotiated")

	type want struct {
		payload []byte
		ts      uint32
	}
	const numPackets = 10
	osnBase := uint16(50000)
	wants := map[uint16]want{} // keyed by OSN (the original media sequence number)
	targetSN := osnBase
	for i := 0; i < numPackets; i++ {
		// build a source packet that carries the padding bit (valid padding trailer)
		// and a distinctive payload.
		payload := distinctivePayload(byte(i+100), 30)
		src := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        true,
				PaddingSize:    20,
				PayloadType:    96,
				SequenceNumber: uint16(2000 + i),
				Timestamp:      uint32(180000 + i*3000),
				SSRC:           0x33333333,
			},
			Payload: payload,
		}
		raw, err := src.Marshal()
		require.NoError(t, err)

		ts := uint32(700000 + i*3000)
		wants[targetSN] = want{payload: payload, ts: ts}

		_, err = dh.dt.RetransmitForTest(uint64(src.SequenceNumber), targetSN, ts, uint64(ts), 0, raw, 0)
		require.NoError(t, err)
		targetSN++
	}

	require.Eventually(t, func() bool {
		return len(cp.rtxPackets()) >= numPackets
	}, 5*time.Second, 20*time.Millisecond, "expected RTX packets to be emitted")

	seen := map[uint16]bool{}
	rtxSeqs := make([]uint16, 0, numPackets)
	for _, pr := range cp.rtxPackets() {
		require.EqualValues(t, dh.dt.SSRCRTX(), pr.hdr.SSRC, "retransmitted packet must be on the RTX SSRC")
		require.EqualValues(t, dh.dt.PayloadTypeRTXForTest(), pr.hdr.PayloadType, "retransmitted packet must use the RTX payload type")
		require.False(t, pr.hdr.Padding, "retransmitted packet must not carry padding bit")
		require.GreaterOrEqual(t, len(pr.payload), 2, "RTX payload must contain the OSN prefix")

		osn := binary.BigEndian.Uint16(pr.payload[0:2])
		w, ok := wants[osn]
		require.True(t, ok, "unexpected OSN %d", osn)
		require.Equal(t, w.payload, pr.payload[2:], "RTX media payload mismatch (osn=%d)", osn)
		require.Equal(t, w.ts, pr.hdr.Timestamp, "RTX timestamp mismatch (osn=%d)", osn)
		seen[osn] = true
		rtxSeqs = append(rtxSeqs, pr.hdr.SequenceNumber)
	}

	for osn := osnBase; osn < osnBase+numPackets; osn++ {
		require.True(t, seen[osn], "OSN %d was not retransmitted via RTX", osn)
	}

	// RTX-stream sequence numbers come from the DownTrack's own RTX sequence space
	// and must advance by one per packet.
	require.Len(t, rtxSeqs, numPackets)
	for i := 1; i < len(rtxSeqs); i++ {
		require.EqualValues(t, rtxSeqs[i-1]+1, rtxSeqs[i], "RTX sequence numbers must be contiguous")
	}
}

// TestDownTrackReplaysPacketTrailerStripOnRetransmit verifies that a packet whose
// packet trailer was stripped when it was forwarded is retransmitted with the same
// bytes removed, i. e. the retransmitted media payload is byte identical to the
// original transmission.
func TestDownTrackReplaysPacketTrailerStripOnRetransmit(t *testing.T) {
	h := vnettest.NewHosts(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	cp := &capturingPacer{inner: pacer.NewPassThrough(logger.GetLogger(), newNullBWE())}
	t.Cleanup(cp.Stop)

	dh := newBoundDownTrack(t, h, factory, vp8CodecParams, cp, vnettest.MediaEngineConfig{Video: true})
	require.NotZero(t, dh.dt.SSRCRTX(), "RTX ssrc should be negotiated")

	video := distinctivePayload(11, 40)
	trailer := lktsTrailer()
	src := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 3000,
			Timestamp:      270000,
			SSRC:           0x44444444,
		},
		Payload: append(append([]byte{}, video...), trailer...),
	}
	raw, err := src.Marshal()
	require.NoError(t, err)

	osn := uint16(60000)
	_, err = dh.dt.RetransmitForTest(
		uint64(src.SequenceNumber),
		osn,
		src.Timestamp,
		uint64(src.Timestamp),
		0,
		raw,
		uint8(len(trailer)),
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(cp.rtxPackets()) >= 1
	}, 5*time.Second, 20*time.Millisecond, "expected RTX packet to be emitted")

	pr := cp.rtxPackets()[0]
	require.EqualValues(t, osn, binary.BigEndian.Uint16(pr.payload[0:2]))
	require.Equal(t, video, pr.payload[2:], "retransmitted payload must not carry the packet trailer")
}

// lktsTrailer builds a 15-byte LKTS packet trailer carrying a user timestamp TLV.
func lktsTrailer() []byte {
	trailer := []byte{0x01 ^ 0xFF, 8 ^ 0xFF}
	for i := 0; i < 8; i++ {
		trailer = append(trailer, byte(i)^0xFF)
	}
	trailer = append(trailer, 15^0xFF)
	return append(trailer, packettrailer.Magic[:]...)
}

// -----------------------------------------------------------------------------
// WritePaddingRTP: padding-only packets
// -----------------------------------------------------------------------------

// TestDownTrackSendsPaddingOnlyPackets exercises the WritePaddingRTP padding-only
// path. It first forwards a video keyframe (verifying the media header/payload),
// then emits padding-only packets and verifies each one on the far side: it is on
// the primary SSRC/payload type, carries the padding bit, and declares a full
// padding block.
//
// A padding-only packet is entirely padding: pion/srtp serializes the padding block
// from header.PaddingSize (via rtp.MarshalPacketTo) after the payload, so the packet
// has no payload. A receiver takes a packet with any payload left after the padding
// as media.
func TestDownTrackSendsPaddingOnlyPackets(t *testing.T) {
	h := vnettest.NewHosts(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	p := pacer.NewPassThrough(logger.GetLogger(), newNullBWE())
	t.Cleanup(p.Stop)

	dh := newBoundDownTrack(t, h, factory, vp8CodecParams, p, vnettest.MediaEngineConfig{Video: true})

	// force a valid target/current layer so the video forwarder will forward (test seam
	// also used by forwarder_test.go's disable()).
	dh.dt.ForceForwardLayerForTest(buffer.VideoLayer{Spatial: 0, Temporal: 0})

	// forward a keyframe so rtpStats becomes active and the stream is running
	vp8 := &codec.VP8{
		FirstByte:  0x10,
		S:          true,
		PictureID:  1,
		IsKeyFrame: true,
	}
	keyframePayload := distinctivePayload(0x40, 50)
	ep, err := testutils.GetTestExtPacketVP8(&testutils.TestExtPacketParams{
		SequenceNumber: 5000,
		Timestamp:      180000,
		SSRC:           0x33333333,
		PayloadType:    96,
		PayloadSize:    50,
		IsKeyFrame:     true,
		Marker:         true, // end of frame, so padding can be emitted on the frame boundary
	}, vp8)
	require.NoError(t, err)
	ep.Packet.Payload = keyframePayload
	require.Equal(t, int32(1), dh.dt.WriteRTP(ep, 0), "keyframe should forward")

	require.True(t, dh.dt.RTPStatsActiveForTest(), "rtpStats should be active after forwarding")

	// wait for the media keyframe to arrive, then take the baseline count.
	require.Eventually(t, func() bool {
		return dh.capture.count() >= 1
	}, 10*time.Second, 20*time.Millisecond, "expected media keyframe to arrive")
	baseline := dh.capture.count()

	// emit padding-only packets. paddingOnMute=true bypasses the RR/active gating so
	// this stays deterministic; the header construction (Padding + PaddingSize) is the
	// same code path either way.
	const numPadding = 3
	sent := 0
	require.Eventually(t, func() bool {
		if dh.dt.WritePaddingRTP(500, true, false) > 0 {
			sent++
		}
		return sent >= numPadding
	}, 5*time.Second, 20*time.Millisecond, "expected WritePaddingRTP to emit padding packets")

	// the padding-only packets must arrive on the far side.
	require.Eventually(t, func() bool {
		return dh.capture.count() >= baseline+numPadding
	}, 10*time.Second, 20*time.Millisecond, "expected padding-only packets to arrive on the far side")

	sawKeyframe := 0
	sawPadding := 0
	for _, pkt := range dh.capture.all() {
		require.EqualValues(t, dh.dt.SSRC(), pkt.SSRC, "packet on wrong SSRC")
		require.EqualValues(t, 96, pkt.PayloadType, "packet has wrong payload type")

		if !pkt.Header.Padding {
			// the forwarded media keyframe
			require.True(t, pkt.Header.Marker, "forwarded keyframe should carry the marker bit")
			// the forwarder prepends the VP8 payload descriptor, so the media bytes
			// appear as a suffix of the forwarded payload.
			require.True(t, bytes.HasSuffix(pkt.Payload, keyframePayload), "forwarded keyframe payload mismatch")
			sawKeyframe++
			continue
		}

		// a padding-only packet: it must declare a full padding block.
		require.EqualValues(t, RTPPaddingMaxPayloadSize, pkt.Header.PaddingSize,
			"padding-only packet must declare full padding size (sn=%d)", pkt.SequenceNumber)
		require.Empty(t, pkt.Payload, "padding-only packet must have no payload (sn=%d)", pkt.SequenceNumber)
		sawPadding++
	}
	require.Equal(t, 1, sawKeyframe, "expected exactly one forwarded media keyframe")
	require.GreaterOrEqual(t, sawPadding, numPadding, "expected the padding-only packets to be observed")

	// the padding in the slot of a dropped packet has no payload either
	dh.dt.WriteDroppedSlotPaddingForTest(6000, 190000)
	var pkt *rtp.Packet
	require.Eventually(t, func() bool {
		for _, p := range dh.capture.all() {
			if p.SequenceNumber == 6000 {
				pkt = p
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond, "expected the dropped slot padding to arrive on the far side")
	require.True(t, pkt.Header.Padding)
	require.EqualValues(t, DroppedSlotPaddingSizeForTest, pkt.Header.PaddingSize)
	require.Empty(t, pkt.Payload)
}

// -----------------------------------------------------------------------------
// WriteProbePackets: RTX probe packets (with StreamAllocator + Pacer + BWE)
// -----------------------------------------------------------------------------

// capturingProbe records what the DownTrack handed the pacer for a probe packet.
type capturingProbe struct {
	hdr        rtp.Header
	payloadLen int
}

// capturedRTX records what the DownTrack handed the pacer for an RTX packet.
type capturedRTX struct {
	hdr     rtp.Header
	payload []byte
}

// capturingPacer wraps a real pacer.Pacer, records probe and RTX packets as the
// DownTrack constructs them (before the real pacer patches/marshals them), and
// delegates everything to the wrapped pacer so packets are still sent over real pion.
type capturingPacer struct {
	inner pacer.Pacer

	lock   sync.Mutex
	probes []capturingProbe
	rtx    []capturedRTX
}

func (c *capturingPacer) Enqueue(p *pacer.Packet) {
	if p.Header != nil {
		switch {
		case p.IsProbe:
			cp := capturingProbe{hdr: *p.Header, payloadLen: len(p.Payload)}
			c.lock.Lock()
			c.probes = append(c.probes, cp)
			c.lock.Unlock()
		case p.IsRTX:
			c.lock.Lock()
			c.rtx = append(c.rtx, capturedRTX{hdr: *p.Header, payload: append([]byte(nil), p.Payload...)})
			c.lock.Unlock()
		}
	}
	c.inner.Enqueue(p)
}

func (c *capturingPacer) probePackets() []capturingProbe {
	c.lock.Lock()
	defer c.lock.Unlock()
	return append([]capturingProbe(nil), c.probes...)
}

func (c *capturingPacer) rtxPackets() []capturedRTX {
	c.lock.Lock()
	defer c.lock.Unlock()
	return append([]capturedRTX(nil), c.rtx...)
}

func (c *capturingPacer) Stop()                       { c.inner.Stop() }
func (c *capturingPacer) SetInterval(i time.Duration) { c.inner.SetInterval(i) }
func (c *capturingPacer) SetBitrate(b int)            { c.inner.SetBitrate(b) }
func (c *capturingPacer) TimeSinceLastSentPacket() time.Duration {
	return c.inner.TimeSinceLastSentPacket()
}
func (c *capturingPacer) SetPacerProbeObserverListener(l pacer.PacerProbeObserverListener) {
	c.inner.SetPacerProbeObserverListener(l)
}
func (c *capturingPacer) StartProbeCluster(pci ccutils.ProbeClusterInfo) {
	c.inner.StartProbeCluster(pci)
}
func (c *capturingPacer) EndProbeCluster(id ccutils.ProbeClusterId) ccutils.ProbeClusterInfo {
	return c.inner.EndProbeCluster(id)
}

// feedReceiverReport hands the DownTrack a synthetic RTCP receiver report for its
// primary SSRC via the same handleRTCP path production uses. This deterministically
// sets LastReceiverReportTime() (a precondition for WriteProbePackets) without
// depending on the timing of the far side's periodic RRs.
func feedReceiverReport(t *testing.T, dt *DownTrack, lastSeqNo uint16) {
	t.Helper()
	rr := &rtcp.ReceiverReport{
		SSRC: 0x9999,
		Reports: []rtcp.ReceptionReport{{
			SSRC:               dt.SSRC(),
			LastSenderReport:   0,
			LastSequenceNumber: uint32(lastSeqNo),
		}},
	}
	raw, err := rr.Marshal()
	require.NoError(t, err)
	dt.HandleRTCPForTest(raw)
	require.NotZero(t, dt.LastReceiverReportTimeForTest(), "synthetic RR should set LastReceiverReportTime")
}

// TestDownTrackSendsProbePackets exercises the WriteProbePackets RTX probe path
// while wiring the DownTrack to a real StreamAllocator + Pacer + BWE. It runs
// against BOTH bandwidth estimators (send-side / remote).
//
// The RTX probe packets are captured at the pacer as the DownTrack builds them (they
// are padding-only on the RTX stream and are not surfaced as a media track by the
// pion receiver, so far-side capture is not reliable for them). Each probe is
// verified to be on the RTX SSRC/payload type, carry the padding bit, declare a full
// padding size, and serialize a full padding block whose trailing byte is the
// padding count.
//
// The media path is exercised end-to-end over real pion with the allocator, the
// pacer and the specific BWE wired in; BWE estimate values themselves are
// intentionally not asserted (they are timing dependent) — the test verifies the
// pipeline stays healthy and the probe serialization is correct with each estimator.
func TestDownTrackSendsProbePackets(t *testing.T) {
	cases := []struct {
		name    string
		makeBWE func() bwe.BWE
	}{
		{
			name: "sendside",
			makeBWE: func() bwe.BWE {
				return sendsidebwe.NewSendSideBWE(sendsidebwe.SendSideBWEParams{
					Config: sendsidebwe.DefaultSendSideBWEConfig,
					Logger: logger.GetLogger(),
				})
			},
		},
		{
			name: "remote",
			makeBWE: func() bwe.BWE {
				return remotebwe.NewRemoteBWE(remotebwe.RemoteBWEParams{
					Config: remotebwe.DefaultRemoteBWEConfig,
					Logger: logger.GetLogger(),
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := vnettest.NewHosts(t)
			factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()

			b := tc.makeBWE()
			cp := &capturingPacer{inner: pacer.NewPassThrough(logger.GetLogger(), b)}
			t.Cleanup(cp.Stop)

			// header extensions (abs-send-time for remote BWE, transport-cc for send-side)
			// are needed for the DownTrack to pick up an ext id, which WriteProbePackets
			// requires.
			dh := newBoundDownTrack(t, h, factory, vp8CodecParams, cp, vnettest.MediaEngineConfig{
				Video:            true,
				HeaderExtensions: true,
			})

			require.NotZero(t, dh.dt.SSRCRTX(), "RTX ssrc should be negotiated")
			require.NotZero(t, dh.dt.PayloadTypeRTXForTest(), "RTX payload type should be negotiated")

			sa := streamallocator.NewStreamAllocator(streamallocator.StreamAllocatorParams{
				Config:    streamallocator.DefaultStreamAllocatorConfig,
				BWE:       b,
				Pacer:     cp,
				RTTGetter: func() (float64, bool) { return 0, false },
				Logger:    logger.GetLogger(),
			}, true, false)
			// AddTrack wires the allocator as the DownTrack's stream-allocator listener
			// (which is what supplies the BWE type used to resolve the abs-send-time /
			// transport-cc ext id that WriteProbePackets requires). We intentionally do
			// NOT Start() the allocator's event loop: its asynchronous re-allocation would
			// race with (and override) the layer we force below, making the forwarding
			// non-deterministic. The probe path is driven directly instead.
			sa.AddTrack(dh.dt, streamallocator.AddTrackParams{
				Source:      livekit.TrackSource_CAMERA,
				PublisherID: "fake-pub",
			})

			// now that the StreamAllocator listener is set and negotiation is complete,
			// (re)resolve the negotiated header extension ids.
			dh.dt.SetRTPHeaderExtensionsForTest()
			require.Eventually(t, func() bool {
				abs, twcc := dh.dt.ExtIDsForTest()
				return abs != 0 || twcc != 0
			}, 3*time.Second, 20*time.Millisecond, "expected an abs-send-time or transport-cc ext id to be negotiated")

			// force a valid target/current layer so the video forwarder will forward.
			dh.dt.ForceForwardLayerForTest(buffer.VideoLayer{Spatial: 0, Temporal: 0})

			// forward a keyframe end-to-end (exercises DownTrack + allocator + pacer + BWE)
			// and verify it arrives with the expected header/payload.
			vp8 := &codec.VP8{FirstByte: 0x10, S: true, PictureID: 1, IsKeyFrame: true}
			keyframePayload := distinctivePayload(0x50, 60)
			ep, err := testutils.GetTestExtPacketVP8(&testutils.TestExtPacketParams{
				SequenceNumber: 6000,
				Timestamp:      270000,
				SSRC:           0x44444444,
				PayloadType:    96,
				PayloadSize:    60,
				IsKeyFrame:     true,
				Marker:         true,
			}, vp8)
			require.NoError(t, err)
			ep.Packet.Payload = keyframePayload
			require.Equal(t, int32(1), dh.dt.WriteRTP(ep, 0), "keyframe should forward")
			require.True(t, dh.dt.RTPStatsActiveForTest())

			require.Eventually(t, func() bool {
				return dh.capture.count() >= 1
			}, 10*time.Second, 20*time.Millisecond, "expected forwarded media to arrive over the full pipeline")

			media := dh.capture.all()[0]
			require.EqualValues(t, dh.dt.SSRC(), media.SSRC, "forwarded media on wrong SSRC")
			require.EqualValues(t, 96, media.PayloadType, "forwarded media has wrong payload type")
			require.False(t, media.Header.Padding, "forwarded media must not carry padding bit")
			// the forwarder prepends the VP8 payload descriptor, so the media bytes
			// appear as a suffix of the forwarded payload.
			require.True(t, bytes.HasSuffix(media.Payload, keyframePayload), "forwarded media payload mismatch")

			// satisfy the RR precondition deterministically, referencing the
			// subscriber-facing sequence number of the forwarded keyframe.
			feedReceiverReport(t, dh.dt, media.SequenceNumber)

			// emit RTX probe packets.
			const numProbe = 3
			require.Eventually(t, func() bool {
				dh.dt.WriteProbePackets(2000, true)
				return len(cp.probePackets()) >= numProbe
			}, 5*time.Second, 20*time.Millisecond, "expected WriteProbePackets to emit RTX probe packets")

			// every probe packet must be on the RTX stream, carry the padding bit, declare
			// the full padding size, and have no payload: pion serializes the padding from
			// the header.
			for _, pr := range cp.probePackets() {
				require.EqualValues(t, 2, pr.hdr.Version, "probe packet version")
				require.EqualValues(t, dh.dt.SSRCRTX(), pr.hdr.SSRC, "probe packet must be on the RTX stream")
				require.EqualValues(t, dh.dt.PayloadTypeRTXForTest(), pr.hdr.PayloadType, "probe packet must use the RTX payload type")
				require.True(t, pr.hdr.Padding, "probe packet must carry padding bit")
				require.EqualValues(t, RTPPaddingMaxPayloadSize, pr.hdr.PaddingSize, "probe packet must set header.PaddingSize")
				require.Zero(t, pr.payloadLen, "probe packet must have no payload")
			}

			// the BWE is wired and exercised (the pacer records every send into it);
			// its state must be queryable without panicking. Exact estimate values are
			// timing dependent and intentionally not asserted.
			require.NotPanics(t, func() { _ = b.CongestionState() })
		})
	}
}

// -----------------------------------------------------------------------------
// keyFrameRequester: PLI only while a layer lock is pending
// -----------------------------------------------------------------------------

// TestDownTrackKeyFrameRequesterArmsOnlyWhilePending verifies that the key frame
// requester keeps sending PLIs while the target layer is not locked, and goes quiet
// once a key frame on the target layer locks it (no periodic wake-ups while locked).
func TestDownTrackKeyFrameRequesterArmsOnlyWhilePending(t *testing.T) {
	h := vnettest.NewHosts(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	p := pacer.NewPassThrough(logger.GetLogger(), newNullBWE())
	t.Cleanup(p.Stop)

	dh := newBoundDownTrack(t, h, factory, vp8CodecParams, p, vnettest.MediaEngineConfig{Video: true})

	// allocate a target layer (spatial 0) without any media, so the lock is pending
	var brs Bitrates
	brs[0][0] = 100_000
	dh.receiver.GetLayeredBitrateReturns([]int32{0}, brs)
	dh.dt.UpTrackMaxPublishedLayerChange(0)
	dh.dt.SetMaxSpatialLayer(0)
	alloc := dh.dt.AllocateOptimal(false, false)
	require.Equal(t, int32(0), alloc.TargetLayer.Spatial, "expected target layer to be allocated")

	// unlocked: PLIs must keep coming at the key frame interval
	require.Eventually(t, func() bool {
		return dh.receiver.SendPLICallCount() >= 3
	}, 5*time.Second, 20*time.Millisecond, "expected repeated PLIs while layer lock is pending")

	// a key frame on the target layer locks it
	vp8 := &codec.VP8{
		FirstByte:  0x10,
		S:          true,
		PictureID:  1,
		IsKeyFrame: true,
	}
	ep, err := testutils.GetTestExtPacketVP8(&testutils.TestExtPacketParams{
		SequenceNumber: 5000,
		Timestamp:      180000,
		SSRC:           0x33333333,
		PayloadType:    96,
		PayloadSize:    50,
		IsKeyFrame:     true,
		Marker:         true,
	}, vp8)
	require.NoError(t, err)
	ep.Packet.Payload = distinctivePayload(0x40, 50)
	require.Equal(t, int32(1), dh.dt.WriteRTP(ep, 0), "keyframe should forward")

	// let an already armed interval fire, then the requester must stay asleep
	time.Sleep(2 * KeyFrameIntervalMaxForTest)
	before := dh.receiver.SendPLICallCount()
	wakeups := dh.dt.KeyFrameRequesterWakeupsForTest()
	time.Sleep(2 * KeyFrameIntervalMaxForTest)
	require.Equal(t, wakeups, dh.dt.KeyFrameRequesterWakeupsForTest(), "key frame requester must not wake up once the layer is locked")
	require.Equal(t, before, dh.receiver.SendPLICallCount(), "no PLI expected once the layer is locked")

	// a resync drops the lock and must wake the requester again
	dh.dt.Resync()
	require.Eventually(t, func() bool {
		return dh.receiver.SendPLICallCount() > before
	}, 5*time.Second, 20*time.Millisecond, "expected PLI after resync")
}

// -----------------------------------------------------------------------------
// pause/resume: probing resumes a track paused by congestion
// -----------------------------------------------------------------------------

// countingPacer counts the bytes handed to the pacer (media, padding and probes),
// which is what a receiver sees arrive on an unconstrained link.
type countingPacer struct {
	pacer.Pacer

	lock    sync.Mutex
	samples []rateSample

	// forwarded media packets, counted here as the far side buffer is unbound and
	// stops surfacing packets after it fills up; the pass-through pacer writes
	// inside Enqueue, so a counted packet is already on the wire
	mediaPackets atomic.Int64
}

type rateSample struct {
	at    time.Time
	bytes int
}

func (c *countingPacer) Enqueue(p *pacer.Packet) {
	size := p.HeaderSize + len(p.Payload)
	if p.Header != nil {
		size += int(p.Header.PaddingSize)
	}
	if !p.IsProbe && !p.IsRTX && len(p.Payload) != 0 {
		c.mediaPackets.Add(1)
	}
	c.lock.Lock()
	c.samples = append(c.samples, rateSample{at: time.Now(), bytes: size})
	c.lock.Unlock()
	c.Pacer.Enqueue(p)
}

// bitrate over the trailing window.
func (c *countingPacer) bitrate(window time.Duration) int64 {
	c.lock.Lock()
	defer c.lock.Unlock()
	cutoff := time.Now().Add(-window)
	total := 0
	keep := c.samples[:0]
	for _, s := range c.samples {
		if s.at.After(cutoff) {
			total += s.bytes
			keep = append(keep, s)
		}
	}
	c.samples = keep
	return int64(float64(total*8) / window.Seconds())
}

// TestDownTrackResumesAfterBandwidthPause reproduces livekit/livekit#5009.
//
// Default server congestion control (remote BWE / REMB, pass-through pacer, padding
// probes) drives a real DownTrack over real pion on vnet. A two layer simulcast
// stream is forwarded, the receiver estimate is capped below the lowest layer for
// 20s, then uncapped. The receiver estimate is a libwebrtc-like REMB model: under the
// cap it reports the cap; after it, the estimate only grows (8%/s) while it is below
// 1.5x the incoming rate + 10 kbps, like AimdRateControl does.
//
// With allow pause the video must come back within a bound after the cap is lifted,
// as it does without allow pause.
func TestDownTrackResumesAfterBandwidthPause(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 60s")
	}

	for _, allowPause := range []bool{false, true} {
		name := "allowPause=false"
		if allowPause {
			name = "allowPause=true"
		}
		t.Run(name, func(t *testing.T) {
			testResumeAfterBandwidthPause(t, allowPause)
		})
	}
}

func testResumeAfterBandwidthPause(t *testing.T, allowPause bool) {
	const (
		layer0Bps = 80_000
		layer1Bps = 170_000
		cappedBps = 60_000 // below the lowest layer, so allow pause pauses
		fps       = 30     // ~one packet per frame, so a layer is ~30 packets/s like a 360p camera
	)

	h := vnettest.NewHosts(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()

	b := remotebwe.NewRemoteBWE(remotebwe.RemoteBWEParams{
		Config: remotebwe.DefaultRemoteBWEConfig,
		Logger: logger.GetLogger(),
	})
	cp := &countingPacer{Pacer: pacer.NewPassThrough(logger.GetLogger(), nil)}
	t.Cleanup(cp.Stop)

	dh := newBoundDownTrack(t, h, factory, vp8CodecParams, cp, vnettest.MediaEngineConfig{
		Video:            true,
		HeaderExtensions: true,
	}, func(r *sfufakes.FakeTrackReceiver) {
		// layer switches without sender reports from the fake publisher
		r.VideoLayerModeReturns(livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR)
	})
	dt := dh.dt

	var brs Bitrates
	brs[0][0] = layer0Bps
	brs[1][0] = layer1Bps
	dh.receiver.GetLayeredBitrateReturns([]int32{0, 1}, brs)

	// publisher side packet buffers, the real bucket at the max video size, so
	// retransmissions and RTX probes read the same history a real receiver has
	var buckets [2]*bucket.Bucket[uint64, uint16]
	var bucketsMu sync.Mutex
	for i := range buckets {
		buckets[i] = bucket.NewBucket[uint64, uint16](500, bucket.RTPMaxPktSize, bucket.RTPSeqNumOffset)
	}
	dh.receiver.ReadRTPCalls(func(buf []byte, layer uint8, esn uint64) (int, error) {
		bucketsMu.Lock()
		defer bucketsMu.Unlock()
		return buckets[layer].GetPacket(buf, esn)
	})

	sa := streamallocator.NewStreamAllocator(streamallocator.StreamAllocatorParams{
		Config:    streamallocator.DefaultStreamAllocatorConfig,
		BWE:       b,
		Pacer:     cp,
		RTTGetter: func() (float64, bool) { return 0.05, true },
		Logger:    logger.GetLogger(),
	}, true, allowPause)
	sa.OnStreamStateChange(func(update *streamallocator.StreamStateUpdate) error {
		logger.Infow("TEST: stream state update", "update", update)
		return nil
	})
	sa.AddTrack(dt, streamallocator.AddTrackParams{
		Source:      livekit.TrackSource_CAMERA,
		PublisherID: "fake-pub",
	})
	dt.SetRTPHeaderExtensionsForTest()
	abs, _ := dt.ExtIDsForTest()
	require.NotZero(t, abs, "abs-send-time must be negotiated for remote BWE")
	sa.Start()
	t.Cleanup(sa.Stop)

	dt.UpTrackMaxPublishedLayerChange(1)
	dt.UpTrackMaxTemporalLayerSeenChange(0)
	dt.SetMaxSpatialLayer(1)
	dt.SetMaxTemporalLayer(0)
	dt.UpTrackLayersChange()
	dt.UpTrackBitrateAvailabilityChange()

	// publisher: both simulcast layers, one packet per frame, key frame every second
	stop := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() { close(stop); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		sizes := []int{layer0Bps / 8 / fps, layer1Bps / 8 / fps}
		var sn [2]uint16
		var ts [2]uint32
		var pid [2]uint16
		sn[1] = 30000
		ticker := time.NewTicker(time.Second / fps)
		defer ticker.Stop()
		frame := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			for layer := int32(0); layer < 2; layer++ {
				sn[layer]++
				ts[layer] += 90000 / fps
				pid[layer]++
				isKey := frame%fps == 0
				ep, err := testutils.GetTestExtPacketVP8(&testutils.TestExtPacketParams{
					SequenceNumber: sn[layer],
					Timestamp:      ts[layer],
					SSRC:           0x1000 + uint32(layer),
					PayloadType:    96,
					PayloadSize:    sizes[layer],
					IsKeyFrame:     isKey,
					Marker:         true,
					ArrivalTime:    time.Now(),
					VideoLayer:     buffer.VideoLayer{Spatial: layer, Temporal: 0},
				}, &codec.VP8{FirstByte: 0x10, S: true, PictureID: pid[layer], IsKeyFrame: isKey})
				if err != nil {
					continue
				}
				bucketsMu.Lock()
				_, _ = buckets[layer].AddPacketWithSequenceNumber(ep.RawPacket, ep.ExtSequenceNumber)
				bucketsMu.Unlock()
				dt.WriteRTP(ep, layer)
			}
			frame++
		}
	}()

	// receiver: libwebrtc-like REMB
	var capped atomic.Bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		est := float64(2_000_000)
		const interval = 200 * time.Millisecond
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			incoming := float64(cp.bitrate(time.Second))
			if capped.Load() {
				est = math.Min(est, cappedBps)
			} else {
				limit := 1.5*incoming + 10_000
				if est < limit {
					est = math.Min(est*(1+0.08*interval.Seconds()), limit)
				}
			}
			remb := &rtcp.ReceiverEstimatedMaximumBitrate{SenderSSRC: 1, Bitrate: float32(est), SSRCs: []uint32{dt.SSRC()}}
			if raw, err := remb.Marshal(); err == nil {
				dt.HandleRTCPForTest(raw)
			}
		}
	}()

	// RR precondition for probes
	require.Eventually(t, func() bool { return dh.capture.count() > 0 }, 10*time.Second, 20*time.Millisecond, "no media forwarded")
	feedReceiverReport(t, dt, dh.capture.all()[0].SequenceNumber)

	mediaRate := func(window time.Duration) int {
		before := cp.mediaPackets.Load()
		time.Sleep(window)
		return int(cp.mediaPackets.Load() - before)
	}

	time.Sleep(3 * time.Second)
	require.Greater(t, mediaRate(time.Second), 5, "media should flow before the cap")

	logger.Infow("TEST: capping")
	capped.Store(true)
	time.Sleep(20 * time.Second)
	cappedPackets := mediaRate(2 * time.Second)
	logger.Infow("TEST: capped media", "packetsIn2s", cappedPackets, "deficient", dt.IsDeficient())
	if allowPause {
		require.Zero(t, cappedPackets, "video should be paused under the cap")
	}

	logger.Infow("TEST: uncapping")
	capped.Store(false)
	uncappedAt := time.Now()
	require.Eventually(t, func() bool {
		return mediaRate(time.Second) >= fps/2
	}, 20*time.Second, 10*time.Millisecond, "video did not resume within 20s of lifting the cap")
	logger.Infow("TEST: resumed", "after", time.Since(uncappedAt))
}
