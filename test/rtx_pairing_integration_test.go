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

package test

// RTX repair stream pairing on simulcast (rid) streams, over a real PCTransport driven
// by a real pion publisher on a virtual network.
//
// RID based simulcast has no a=ssrc-group:FID line, so the pairing cannot come from
// SDP. The repair SSRC of a layer comes either from the mid/rsid header extensions on
// its packets, or - for a migrated publisher, which is mid-stream and no longer sends
// those extensions - from the migration info in TransportParams.SimTracks.
//
// Neither source fails loudly when it breaks: the repair buffer just accumulates
// packets that are never applied and NACK recovery for simulcast stops working. Both
// paths are therefore asserted end to end, by retransmitting a sequence number that is
// never sent on the primary stream and requiring it to surface on the primary buffer.

import (
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v4/vnet"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/transport/transportfakes"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	sfuinterceptor "github.com/livekit/livekit-server/pkg/sfu/interceptor"
	"github.com/livekit/livekit-server/pkg/testutils/vnettest"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// the single video m-line of the publisher's offer
const rtxTestMid = "0"

const (
	sendsExtensions = false
	omitsExtensions = true
)

// SSRCs are chosen by the test rather than taken from the offer; see stripDeclaredSSRCs.
var rtxTestLayers = []struct {
	rid          string
	ssrc         uint32
	rtxSSRC      uint32
	recoveredSeq uint16
}{
	{rid: "q", ssrc: 1001, rtxSSRC: 2001, recoveredSeq: 50001},
	{rid: "h", ssrc: 1002, rtxSSRC: 2002, recoveredSeq: 50002},
	{rid: "f", ssrc: 1003, rtxSSRC: 2003, recoveredSeq: 50003},
}

// TestSimulcastRTXPairing covers a publisher sending mid/rid/rsid: the pairing comes
// from probing the packets.
func TestSimulcastRTXPairing(t *testing.T) {
	h := newRTXHarness(t, nil)

	h.run(t, sendsExtensions)

	// pairing is also reported through the callback mediatrack subscribes to
	require.Equal(t, len(rtxTestLayers), h.tracker.pairCount(), "not all rtx pairs found: %s", h.tracker.describe())
	for _, w := range h.writers {
		base, repair := h.tracker.pair(w.rid)
		require.Equal(t, w.ssrc, base, "wrong base ssrc paired for rid %q", w.rid)
		require.Equal(t, w.rtxSSRC, repair, "wrong repair ssrc paired for rid %q", w.rid)
	}
}

// TestSimulcastRTXPairingAfterMigration covers a migrated publisher: it is mid-stream
// and sends no mid/rid/rsid, so UnhandleSimulcastInterceptor synthesises them for pion
// and the pairing has to come from SimTracks. RepairSSRC names the repair stream.
func TestSimulcastRTXPairingAfterMigration(t *testing.T) {
	simTracks := make(map[uint32]sfuinterceptor.SimulcastTrackInfo, 2*len(rtxTestLayers))
	for _, l := range rtxTestLayers {
		simTracks[l.ssrc] = sfuinterceptor.SimulcastTrackInfo{
			Mid:        rtxTestMid,
			StreamID:   l.rid,
			RepairSSRC: l.rtxSSRC,
		}
		simTracks[l.rtxSSRC] = sfuinterceptor.SimulcastTrackInfo{
			Mid:            rtxTestMid,
			StreamID:       l.rid,
			IsRepairStream: true,
		}
	}

	newRTXHarness(t, simTracks).run(t, omitsExtensions)
}

// TestSimulcastRTXPairingAfterMigrationWithoutRepairSSRC covers migration info that
// marks the repair stream but leaves RepairSSRC unset on the primary entry.
func TestSimulcastRTXPairingAfterMigrationWithoutRepairSSRC(t *testing.T) {
	simTracks := make(map[uint32]sfuinterceptor.SimulcastTrackInfo, 2*len(rtxTestLayers))
	for _, l := range rtxTestLayers {
		simTracks[l.ssrc] = sfuinterceptor.SimulcastTrackInfo{Mid: rtxTestMid, StreamID: l.rid}
		simTracks[l.rtxSSRC] = sfuinterceptor.SimulcastTrackInfo{
			Mid:            rtxTestMid,
			StreamID:       l.rid,
			IsRepairStream: true,
		}
	}

	newRTXHarness(t, simTracks).run(t, omitsExtensions)
}

// -----------------------------------------------------------------------------
// harness
// -----------------------------------------------------------------------------

type rtxHarness struct {
	transport *rtc.PCTransport
	pubPC     *webrtc.PeerConnection
	writers   []*simulcastWriter
	tracker   *rtxPairTracker
}

func newRTXHarness(t *testing.T, simTracks map[uint32]sfuinterceptor.SimulcastTrackInfo) *rtxHarness {
	t.Helper()

	hosts := vnettest.NewHosts(t)
	tracker := newRTXPairTracker()

	bufferFactory := buffer.NewFactoryOfBufferFactory(500, 200).CreateBufferFactory()
	pcTransport := newPublisherTransportForTest(t, hosts.AnswerNet, bufferFactory, simTracks, tracker)
	pubPC, writers := newSimulcastPublisherPC(t, hosts.OfferNet)

	return &rtxHarness{
		transport: pcTransport,
		pubPC:     pubPC,
		writers:   writers,
		tracker:   tracker,
	}
}

// run negotiates, publishes every layer, then retransmits a sequence number that is
// never sent on the primary stream and waits for it to surface on the primary buffer.
func (h *rtxHarness) run(t *testing.T, omitExtensions bool) {
	t.Helper()

	signalToTransport(t, h.pubPC, h.transport)
	require.Equal(t, rtxTestMid, h.pubPC.GetTransceivers()[0].Mid())
	for _, w := range h.writers {
		w.mid = rtxTestMid
		w.omitExtensions = omitExtensions
	}

	// every layer has to bind before RTX is sent, which is also the production ordering:
	// a retransmission only follows a NACK for an established layer
	require.True(
		t,
		sendUntil(t, 20*time.Second, func() bool { return h.tracker.boundCount() == len(rtxTestLayers) }, func() {
			for _, w := range h.writers {
				w.writePrimary(t)
			}
		}),
		"timed out waiting for all simulcast layers to bind: %s", h.tracker.describe(),
	)

	require.True(
		t,
		sendUntil(t, 20*time.Second, func() bool {
			for _, w := range h.writers {
				if !h.tracker.sawSeq(w.rid, w.recoveredSeq) {
					return false
				}
			}
			return true
		}, func() {
			for _, w := range h.writers {
				w.writeRepair(t, h.tracker.rtxPayloadType(w.rid), w.recoveredSeq)
			}
		}),
		"retransmissions never recovered into the primary buffers: %s", h.tracker.describe(),
	)
}

// newPublisherTransportForTest builds the production publisher transport on net and
// wires up what ParticipantImpl/MediaTrack do with a published layer.
func newPublisherTransportForTest(
	t *testing.T,
	net *vnet.Net,
	bufferFactory *buffer.Factory,
	simTracks map[uint32]sfuinterceptor.SimulcastTrackInfo,
	tracker *rtxPairTracker,
) *rtc.PCTransport {
	t.Helper()

	rtcConf := newVNetWebRTCConfig(t, net, bufferFactory)

	handler := &transportfakes.FakeHandler{}
	params := rtc.TransportParams{
		Handler:         handler,
		Config:          rtcConf,
		DirectionConfig: rtcConf.Publisher,
		ProtocolVersion: 6,
		Logger:          logger.GetLogger(),
		Transport:       livekit.SignalTarget_PUBLISHER,
		SimTracks:       simTracks,
		EnabledPublishCodecs: []*livekit.Codec{
			{Mime: webrtc.MimeTypeVP8},
			{Mime: webrtc.MimeTypeRTX},
		},
		// all candidates are carried in the answer, so the test needs no trickle
		UseOneShotSignallingMode: true,
	}

	pcTransport, err := rtc.NewPCTransport(params)
	require.NoError(t, err)
	t.Cleanup(pcTransport.Close)

	// mirror mediatrack.addReceiver: bind the buffer of each published layer, subscribe
	// to the pairing notification, and drain the buffer the way WebRTCReceiver does
	handler.OnTrackCalls(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		rid, ssrc := track.RID(), uint32(track.SSRC())

		buff := bufferFactory.GetBuffer(ssrc)
		if buff == nil {
			t.Errorf("no buffer for published ssrc %d (rid %q)", ssrc, rid)
			return
		}
		if err := buff.Bind(receiver.GetParameters(), track.Codec().RTPCodecCapability, 0); err != nil {
			t.Errorf("binding buffer for rid %q failed: %v", rid, err)
			return
		}
		buff.OnNotifyRTX(func(base, repair uint32, rsid string) {
			tracker.pairFound(rsid, base, repair)
		})

		// mirror ParticipantImpl.onMediaTrack
		pcTransport.RTPStreamPublished(ssrc, pcTransport.GetMid(receiver), rid)

		tracker.layerBound(rid, ssrc, buff, receiver.GetParameters())
		go tracker.drain(rid, buff)
	})

	return pcTransport
}

// signalToTransport runs a one-shot offer/answer against the transport and waits for
// the publisher to connect.
func signalToTransport(t *testing.T, pub *webrtc.PeerConnection, pcTransport *rtc.PCTransport) {
	t.Helper()

	connected := vnettest.UntilConnected(pub)

	offer := vnettest.GatheredOffer(t, pub)
	offer.SDP = stripDeclaredSSRCs(offer.SDP)
	require.NoError(t, pcTransport.HandleRemoteDescription(offer, 1))

	answer, _, err := pcTransport.GetAnswer()
	require.NoError(t, err)
	require.NoError(t, pub.SetRemoteDescription(answer))

	select {
	case <-connected:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the publisher to connect")
	}
}

// -----------------------------------------------------------------------------
// publisher: raw simulcast writer with a per-layer repair stream
// -----------------------------------------------------------------------------

// rawTrackLocal hands the test the negotiated write stream directly. Unlike
// TrackLocalStaticRTP it does not rewrite SSRC or payload type, which is what lets a
// repair stream be emitted on its own SSRC alongside the primary stream of the same rid.
type rawTrackLocal struct {
	id       string
	streamID string
	rid      string

	lock    sync.Mutex
	writers []webrtc.TrackLocalWriter
	exts    []webrtc.RTPHeaderExtensionParameter
}

func (t *rawTrackLocal) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	for _, c := range ctx.CodecParameters() {
		if c.PayloadType != vnettest.VP8PayloadType {
			continue
		}

		t.lock.Lock()
		t.writers = append(t.writers, ctx.WriteStream())
		t.exts = ctx.HeaderExtensions()
		t.lock.Unlock()
		return c, nil
	}
	return webrtc.RTPCodecParameters{}, fmt.Errorf("vp8 not negotiated for rid %q", t.rid)
}

func (t *rawTrackLocal) Unbind(webrtc.TrackLocalContext) error { return nil }
func (t *rawTrackLocal) ID() string                            { return t.id }
func (t *rawTrackLocal) RID() string                           { return t.rid }
func (t *rawTrackLocal) StreamID() string                      { return t.streamID }
func (t *rawTrackLocal) Kind() webrtc.RTPCodecType             { return webrtc.RTPCodecTypeVideo }

func (t *rawTrackLocal) extensionID(uri string) uint8 {
	t.lock.Lock()
	defer t.lock.Unlock()

	for _, e := range t.exts {
		if e.URI == uri {
			return uint8(e.ID)
		}
	}
	return 0
}

func (t *rawTrackLocal) write(header *rtp.Header, payload []byte) {
	t.lock.Lock()
	writers := append([]webrtc.TrackLocalWriter(nil), t.writers...)
	t.lock.Unlock()

	for _, w := range writers {
		_, _ = w.WriteRTP(header, payload)
	}
}

// simulcastWriter emits the primary and repair streams of one simulcast layer.
type simulcastWriter struct {
	track   *rawTrackLocal
	mid     string
	rid     string
	ssrc    uint32
	rtxSSRC uint32

	// omitExtensions emulates a migrated publisher, which sends no mid/rid/rsid
	omitExtensions bool

	// recoveredSeq is only ever sent inside an RTX payload, never on the primary
	// stream, so its arrival on the primary buffer proves RTX recovery worked
	recoveredSeq uint16

	lock sync.Mutex
	seq  uint16
}

func (w *simulcastWriter) nextSeq() uint16 {
	w.lock.Lock()
	defer w.lock.Unlock()

	w.seq++
	return w.seq
}

func (w *simulcastWriter) header(t *testing.T, ssrc uint32, pt uint8, seq uint16, rid, rsid string) *rtp.Header {
	t.Helper()

	h := &rtp.Header{
		Version:        2,
		PayloadType:    pt,
		SequenceNumber: seq,
		Timestamp:      uint32(seq) * 3000,
		SSRC:           ssrc,
	}
	if w.omitExtensions {
		return h
	}

	midID := w.track.extensionID(sdp.SDESMidURI)
	require.NotZero(t, midID, "sdes:mid not negotiated")
	require.NoError(t, h.SetExtension(midID, []byte(w.mid)))

	if rid != "" {
		ridID := w.track.extensionID(sdp.SDESRTPStreamIDURI)
		require.NotZero(t, ridID, "sdes:rtp-stream-id not negotiated")
		require.NoError(t, h.SetExtension(ridID, []byte(rid)))
	}
	if rsid != "" {
		rsidID := w.track.extensionID(sdp.SDESRepairRTPStreamIDURI)
		require.NotZero(t, rsidID, "sdes:repaired-rtp-stream-id not negotiated")
		require.NoError(t, h.SetExtension(rsidID, []byte(rsid)))
	}
	return h
}

func (w *simulcastWriter) writePrimary(t *testing.T) {
	t.Helper()

	w.track.write(w.header(t, w.ssrc, vnettest.VP8PayloadType, w.nextSeq(), w.rid, ""), vp8TestPayload())
}

// writeRepair emits an RFC 4588 repair packet: the sequence number being retransmitted
// is prepended to the payload.
func (w *simulcastWriter) writeRepair(t *testing.T, rtxPT uint8, originalSeq uint16) {
	t.Helper()

	if rtxPT == 0 {
		rtxPT = vnettest.RTXPayloadType
	}

	inner := vp8TestPayload()
	payload := make([]byte, 2+len(inner))
	binary.BigEndian.PutUint16(payload[:2], originalSeq)
	copy(payload[2:], inner)

	w.track.write(w.header(t, w.rtxSSRC, rtxPT, w.nextSeq(), "", w.rid), payload)
}

func vp8TestPayload() []byte {
	return []byte{0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0x40, 0x01, 0xf0, 0x00}
}

// newSimulcastPublisherPC builds the publishing peer connection and one writer per
// simulcast layer.
func newSimulcastPublisherPC(t *testing.T, net *vnet.Net) (*webrtc.PeerConnection, []*simulcastWriter) {
	t.Helper()

	pc := vnettest.NewPeerConnection(t, vnettest.PCConfig{
		Net: net,
		MediaEngine: vnettest.MediaEngineConfig{
			Video:               true,
			HeaderExtensions:    true,
			SimulcastExtensions: true,
		},
	})

	writers := make([]*simulcastWriter, 0, len(rtxTestLayers))
	for _, l := range rtxTestLayers {
		writers = append(writers, &simulcastWriter{
			track:        &rawTrackLocal{id: "video", streamID: "pion", rid: l.rid},
			rid:          l.rid,
			ssrc:         l.ssrc,
			rtxSSRC:      l.rtxSSRC,
			recoveredSeq: l.recoveredSeq,
		})
	}

	sender, err := pc.AddTrack(writers[0].track)
	require.NoError(t, err)
	for _, w := range writers[1:] {
		require.NoError(t, sender.AddEncoding(w.track))
	}

	return pc, writers
}

// -----------------------------------------------------------------------------
// tracking
// -----------------------------------------------------------------------------

type rtxPairTracker struct {
	lock sync.Mutex

	bound  map[string]uint32 // rid -> base ssrc
	params map[string]webrtc.RTPParameters
	pairs  map[string][2]uint32       // rsid -> {base ssrc, repair ssrc}
	seen   map[string]map[uint16]bool // rid -> sequence numbers read off the primary buffer
}

func newRTXPairTracker() *rtxPairTracker {
	return &rtxPairTracker{
		bound:  make(map[string]uint32),
		params: make(map[string]webrtc.RTPParameters),
		pairs:  make(map[string][2]uint32),
		seen:   make(map[string]map[uint16]bool),
	}
}

func (t *rtxPairTracker) layerBound(rid string, ssrc uint32, buff *buffer.Buffer, params webrtc.RTPParameters) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.bound[rid] = ssrc
	t.params[rid] = params
	t.seen[rid] = make(map[uint16]bool)
}

func (t *rtxPairTracker) pairFound(rsid string, base, repair uint32) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.pairs[rsid] = [2]uint32{base, repair}
}

func (t *rtxPairTracker) boundCount() int {
	t.lock.Lock()
	defer t.lock.Unlock()

	return len(t.bound)
}

func (t *rtxPairTracker) pairCount() int {
	t.lock.Lock()
	defer t.lock.Unlock()

	return len(t.pairs)
}

func (t *rtxPairTracker) pair(rid string) (uint32, uint32) {
	t.lock.Lock()
	defer t.lock.Unlock()

	p := t.pairs[rid]
	return p[0], p[1]
}

func (t *rtxPairTracker) rtxPayloadType(rid string) uint8 {
	t.lock.Lock()
	defer t.lock.Unlock()

	for _, c := range t.params[rid].Codecs {
		if c.MimeType == webrtc.MimeTypeRTX {
			return uint8(c.PayloadType)
		}
	}
	return 0
}

func (t *rtxPairTracker) sawSeq(rid string, seq uint16) bool {
	t.lock.Lock()
	defer t.lock.Unlock()

	return t.seen[rid][seq]
}

// drain consumes the primary buffer the way WebRTCReceiver does, recording which
// sequence numbers made it through.
func (t *rtxPairTracker) drain(rid string, buff *buffer.Buffer) {
	b := make([]byte, 1500)
	for {
		ep, err := buff.ReadExtended(b)
		if err != nil {
			return
		}
		if ep == nil || ep.Packet == nil {
			continue
		}

		t.lock.Lock()
		t.seen[rid][ep.Packet.SequenceNumber] = true
		t.lock.Unlock()
	}
}

func (t *rtxPairTracker) describe() string {
	t.lock.Lock()
	defer t.lock.Unlock()

	return fmt.Sprintf("bound=%v pairs=%v", t.bound, t.pairs)
}
