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
// pion's rtp.Packet.Unmarshal rejects a packet whose padding bit is set but whose
// trailing padding-size byte is invalid/zero (errInvalidRTPPadding), so a
// malformed packet is dropped by the receiver and never arrives. Arrival on the
// far side is therefore itself evidence that the packet was serialized correctly.

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v4/vnet"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

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
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
	"github.com/livekit/livekit-server/pkg/sfu/streamallocator"
	"github.com/livekit/livekit-server/pkg/sfu/testutils"
)

// -----------------------------------------------------------------------------
// vnet harness
// -----------------------------------------------------------------------------

type vnetHarness struct {
	wan       *vnet.Router
	offerNet  *vnet.Net
	answerNet *vnet.Net
}

func buildVNet(t *testing.T) *vnetHarness {
	t.Helper()

	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          "1.2.3.0/24",
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	require.NoError(t, err)

	offerNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"1.2.3.4"}})
	require.NoError(t, err)
	require.NoError(t, wan.AddNet(offerNet))

	answerNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"1.2.3.5"}})
	require.NoError(t, err)
	require.NoError(t, wan.AddNet(answerNet))

	require.NoError(t, wan.Start())
	t.Cleanup(func() { _ = wan.Stop() })

	return &vnetHarness{wan: wan, offerNet: offerNet, answerNet: answerNet}
}

// mediaEngineConfig describes what codecs / header extensions to register on a PC.
type mediaEngineConfig struct {
	video            bool // register VP8 (+ RTX); otherwise register opus
	headerExtensions bool // register abs-send-time + transport-cc header extensions
}

func newMediaPC(t *testing.T, net *vnet.Net, factory *buffer.Factory, cfg mediaEngineConfig) *webrtc.PeerConnection {
	t.Helper()

	me := &webrtc.MediaEngine{}
	if cfg.video {
		require.NoError(t, me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: videoRTCPFeedback(),
			},
			PayloadType: 96,
		}, webrtc.RTPCodecTypeVideo))
		require.NoError(t, me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: "apt=96",
			},
			PayloadType: 97,
		}, webrtc.RTPCodecTypeVideo))
	} else {
		require.NoError(t, me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			},
			PayloadType: 111,
		}, webrtc.RTPCodecTypeAudio))
	}

	if cfg.headerExtensions {
		kind := webrtc.RTPCodecTypeAudio
		if cfg.video {
			kind = webrtc.RTPCodecTypeVideo
		}
		require.NoError(t, me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.ABSSendTimeURI}, kind))
		require.NoError(t, me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, kind))
	}

	// no pion default interceptors: the DownTrack/pacer fill abs-send-time and
	// transport-cc themselves, and the tests drive RTCP feedback synthetically, so
	// pion's own feedback generators would only add nondeterminism.
	ir := &interceptor.Registry{}

	se := webrtc.SettingEngine{}
	se.SetNet(net)
	se.SetICETimeouts(time.Second, time.Second, 200*time.Millisecond)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	if factory != nil {
		se.BufferFactory = factory.GetOrNew
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(se),
	)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })

	return pc
}

func videoRTCPFeedback() []webrtc.RTCPFeedback {
	return []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: webrtc.TypeRTCPFBTransportCC},
		{Type: webrtc.TypeRTCPFBGoogREMB},
	}
}

// signalPair performs a full offer/answer exchange between two PCs (adapted from
// pion's own test helper) and waits for both to reach the connected state.
func signalPair(t *testing.T, offerer, answerer *webrtc.PeerConnection) {
	t.Helper()

	connected := untilConnected(offerer, answerer)

	offer, err := offerer.CreateOffer(nil)
	require.NoError(t, err)
	gatherOffer := webrtc.GatheringCompletePromise(offerer)
	require.NoError(t, offerer.SetLocalDescription(offer))
	<-gatherOffer

	require.NoError(t, answerer.SetRemoteDescription(*offerer.LocalDescription()))

	answer, err := answerer.CreateAnswer(nil)
	require.NoError(t, err)
	gatherAnswer := webrtc.GatheringCompletePromise(answerer)
	require.NoError(t, answerer.SetLocalDescription(answer))
	<-gatherAnswer

	require.NoError(t, offerer.SetRemoteDescription(*answerer.LocalDescription()))

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for peer connections to connect")
	}
}

func untilConnected(pcs ...*webrtc.PeerConnection) <-chan struct{} {
	var wg sync.WaitGroup
	wg.Add(len(pcs))
	for _, pc := range pcs {
		var once sync.Once
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			if s == webrtc.PeerConnectionStateConnected {
				once.Do(wg.Done)
			}
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
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
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: videoRTCPFeedback()},
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
func newBoundDownTrack(t *testing.T, h *vnetHarness, factory *buffer.Factory, codecParams webrtc.RTPCodecParameters, p pacer.Pacer, meCfg mediaEngineConfig) *downTrackHarness {
	t.Helper()

	sender := newMediaPC(t, h.offerNet, factory, meCfg)
	sub := newMediaPC(t, h.answerNet, factory, meCfg)
	capture := captureTrack(sub)

	rcv := newFakeTrackReceiver(codecParams)
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

	signalPair(t, sender, sub)
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
	h := buildVNet(t)

	sender := newMediaPC(t, h.offerNet, nil, mediaEngineConfig{video: true})
	receiver := newMediaPC(t, h.answerNet, nil, mediaEngineConfig{video: true})

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "pion-spike",
	)
	require.NoError(t, err)
	_, err = sender.AddTrack(track)
	require.NoError(t, err)

	cap := captureTrack(receiver)

	signalPair(t, sender, receiver)

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
	h := buildVNet(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	p := pacer.NewPassThrough(logger.GetLogger(), newNullBWE())
	t.Cleanup(p.Stop)

	dh := newBoundDownTrack(t, h, factory, opusCodecParams, p, mediaEngineConfig{video: false})

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

// -----------------------------------------------------------------------------
// retransmitPacket: RTX / retransmission
// -----------------------------------------------------------------------------

// TestDownTrackRetransmitsPackets exercises the retransmit path and verifies the
// retransmitted header/payload on the far side: SSRC, payload type, sequence
// number, timestamp, payload bytes, and that the padding bit is cleared regardless
// of the source packet's padding bit.
func TestDownTrackRetransmitsPackets(t *testing.T) {
	h := buildVNet(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	p := pacer.NewPassThrough(logger.GetLogger(), newNullBWE())
	t.Cleanup(p.Stop)

	dh := newBoundDownTrack(t, h, factory, opusCodecParams, p, mediaEngineConfig{video: false})

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

// -----------------------------------------------------------------------------
// WritePaddingRTP: padding-only packets
// -----------------------------------------------------------------------------

// TestDownTrackSendsPaddingOnlyPackets exercises the WritePaddingRTP padding-only
// path. It first forwards a video keyframe (verifying the media header/payload),
// then emits padding-only packets and verifies each one on the far side: it is on
// the primary SSRC/payload type, carries the padding bit, and declares a full
// padding block.
//
// A padding-only packet is entirely padding; pion/srtp serializes the padding block
// from header.PaddingSize (via rtp.MarshalPacketTo), and the pion receiver strips it
// and reports both the padding bit and the padding size, so the received payload
// length equals the declared padding size (RTPPaddingMaxPayloadSize).
func TestDownTrackSendsPaddingOnlyPackets(t *testing.T) {
	h := buildVNet(t)
	factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()
	p := pacer.NewPassThrough(logger.GetLogger(), newNullBWE())
	t.Cleanup(p.Stop)

	dh := newBoundDownTrack(t, h, factory, vp8CodecParams, p, mediaEngineConfig{video: true})

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
		require.Equal(t, RTPPaddingMaxPayloadSize, len(pkt.Payload),
			"padding-only packet must serialize a full padding block (sn=%d)", pkt.SequenceNumber)
		sawPadding++
	}
	require.Equal(t, 1, sawKeyframe, "expected exactly one forwarded media keyframe")
	require.GreaterOrEqual(t, sawPadding, numPadding, "expected the padding-only packets to be observed")
}

// -----------------------------------------------------------------------------
// WriteProbePackets: RTX probe packets (with StreamAllocator + Pacer + BWE)
// -----------------------------------------------------------------------------

// capturingProbe records what the DownTrack handed the pacer for a probe packet.
type capturingProbe struct {
	hdr        rtp.Header
	payloadLen int
	lastByte   byte
}

// capturingPacer wraps a real pacer.Pacer, records probe packets as the DownTrack
// constructs them (before the real pacer patches/marshals them), and delegates
// everything to the wrapped pacer so packets are still sent over real pion.
type capturingPacer struct {
	inner pacer.Pacer

	lock   sync.Mutex
	probes []capturingProbe
}

func (c *capturingPacer) Enqueue(p *pacer.Packet) {
	if p.IsProbe && p.Header != nil {
		cp := capturingProbe{hdr: *p.Header, payloadLen: len(p.Payload)}
		if len(p.Payload) > 0 {
			cp.lastByte = p.Payload[len(p.Payload)-1]
		}
		c.lock.Lock()
		c.probes = append(c.probes, cp)
		c.lock.Unlock()
	}
	c.inner.Enqueue(p)
}

func (c *capturingPacer) probePackets() []capturingProbe {
	c.lock.Lock()
	defer c.lock.Unlock()
	return append([]capturingProbe(nil), c.probes...)
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
			h := buildVNet(t)
			factory := buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory()

			b := tc.makeBWE()
			cp := &capturingPacer{inner: pacer.NewPassThrough(logger.GetLogger(), b)}
			t.Cleanup(cp.Stop)

			// header extensions (abs-send-time for remote BWE, transport-cc for send-side)
			// are needed for the DownTrack to pick up an ext id, which WriteProbePackets
			// requires.
			dh := newBoundDownTrack(t, h, factory, vp8CodecParams, cp, mediaEngineConfig{
				video:            true,
				headerExtensions: true,
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
			// the full padding size, and serialize a full padding block ending in the
			// padding count byte.
			for _, pr := range cp.probePackets() {
				require.EqualValues(t, 2, pr.hdr.Version, "probe packet version")
				require.EqualValues(t, dh.dt.SSRCRTX(), pr.hdr.SSRC, "probe packet must be on the RTX stream")
				require.EqualValues(t, dh.dt.PayloadTypeRTXForTest(), pr.hdr.PayloadType, "probe packet must use the RTX payload type")
				require.True(t, pr.hdr.Padding, "probe packet must carry padding bit")
				require.EqualValues(t, RTPPaddingMaxPayloadSize, pr.hdr.PaddingSize, "probe packet must set header.PaddingSize")
				require.Equal(t, RTPPaddingMaxPayloadSize, pr.payloadLen, "probe packet must carry a full padding block")
				require.EqualValues(t, RTPPaddingMaxPayloadSize, pr.lastByte, "probe packet trailing byte must be the padding count")
			}

			// the BWE is wired and exercised (the pacer records every send into it);
			// its state must be queryable without panicking. Exact estimate values are
			// timing dependent and intentionally not asserted.
			require.NotPanics(t, func() { _ = b.CongestionState() })
		})
	}
}
