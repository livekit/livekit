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

package media

import (
	"bytes"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/codec"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/packettrailer"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector/temporallayerselector"
)

// vp9Keyframe is a VP9 profile 0 keyframe uncompressed header for 320x240
// (frame marker, sync code, color config, frame size), padded.
func vp9Keyframe(size int) []byte {
	b := []byte{0x82, 0x49, 0x83, 0x42, 0x00, 0x13, 0xf0, 0x0e, 0xf6}
	for len(b) < size {
		b = append(b, byte(len(b)))
	}
	return b
}

func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

// extPackets turns RTP payloads into the ExtPackets the egress tap sees, with
// the buffer's view of the VP9 descriptor and keyframes.
func extPackets(t *testing.T, m mime.MimeType, payloads [][]byte, seq uint64, ts uint64, bufferKeyframe func(payload []byte) bool) []*buffer.ExtPacket {
	var out []*buffer.ExtPacket
	for i, pl := range payloads {
		ext := &buffer.ExtPacket{
			Packet:            &rtp.Packet{Header: rtp.Header{Marker: i == len(payloads)-1, SequenceNumber: uint16(seq) + uint16(i), Timestamp: uint32(ts)}, Payload: pl},
			ExtSequenceNumber: seq + uint64(i),
			ExtTimestamp:      ts,
			IsKeyFrame:        bufferKeyframe(pl),
		}
		ext.VideoLayer.Spatial, ext.VideoLayer.Temporal = -1, -1
		if m == mime.MimeTypeVP9 {
			vp9 := &codecs.VP9Packet{}
			_, err := vp9.Unmarshal(pl)
			require.NoError(t, err)
			ext.VP9 = vp9
			ext.VideoLayer.Spatial, ext.VideoLayer.Temporal = int32(vp9.SID), int32(vp9.TID)
		}
		out = append(out, ext)
	}
	return out
}

func assemble(t *testing.T, a *Assembler, m mime.MimeType, exts []*buffer.ExtPacket, encrypted bool) []Frame {
	var frames []Frame
	for _, ext := range exts {
		frames = append(frames, a.Push(PacketFromExt(ext, m, nil, encrypted), time.Now())...)
	}
	return frames
}

// The VP9 packetizer writes flexible-mode descriptors from frame metadata:
// libwebrtc receivers drop non-flexible frames without TL0PICIDX, and an
// encrypted payload cannot be parsed.
func TestVP9PacketizerFlexibleMode(t *testing.T) {
	p := &VP9Packetizer{PictureID: 0x7ffe}
	key := vp9Keyframe(3000)
	payloads := p.Packetize(1188, key, &IngestFrame{Keyframe: true})
	require.Len(t, payloads, 3)
	for i, pl := range payloads {
		require.LessOrEqual(t, len(pl), 1188)
		var vp9 codecs.VP9Packet
		_, err := vp9.Unmarshal(pl)
		require.NoError(t, err)
		require.True(t, vp9.F, "flexible mode")
		require.True(t, vp9.I)
		require.Equal(t, uint16(0x7ffe), vp9.PictureID)
		require.False(t, vp9.P, "keyframe is not inter-picture predicted")
		require.Equal(t, i == 0, vp9.B)
		require.Equal(t, i == len(payloads)-1, vp9.E)
		require.Equal(t, i == 0, vp9.V, "scalability structure on the first packet")
		if vp9.V {
			require.Equal(t, []uint16{320}, vp9.Width)
			require.Equal(t, []uint16{240}, vp9.Height)
		}
	}
	// the SFU buffer recognises the keyframe from the bitstream
	require.True(t, codec.IsVP9KeyFrame(nil, payloads[0]))

	delta := []byte{0x86, 1, 2, 3, 4}
	payloads = p.Packetize(1188, delta, &IngestFrame{})
	require.Len(t, payloads, 1)
	var vp9 codecs.VP9Packet
	_, err := vp9.Unmarshal(payloads[0])
	require.NoError(t, err)
	require.True(t, vp9.P)
	require.Equal(t, []uint8{1}, vp9.PDiff, "references the previous picture")
	require.Equal(t, uint16(0x7fff), vp9.PictureID)
	require.Equal(t, delta, vp9.Payload)

	// the picture id wraps at 15 bits
	payloads = p.Packetize(1188, delta, &IngestFrame{})
	_, err = vp9.Unmarshal(payloads[0])
	require.NoError(t, err)
	require.Equal(t, uint16(0), vp9.PictureID)
}

// Encrypted VP9 and AV1 (E2EE encrypts them from the first byte) cross the
// SFU byte for byte: ingest packetizes them without parsing, egress finds
// keyframes in the payload descriptor rather than the bitstream.
func TestEncryptedVP9AndAV1RoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, m := range []mime.MimeType{mime.MimeTypeVP9, mime.MimeTypeAV1} {
		t.Run(m.String(), func(t *testing.T) {
			packetize := newFramePacketizer(m, true)
			a := NewAssembler(m, AssemblerOptions{Encrypted: true})
			seq, ts := uint64(1000), uint64(90000)
			var sent [][]byte
			var got []Frame
			for i := 0; i < 20; i++ {
				key := i%10 == 0
				frame := randomBytes(r, 200+r.Intn(4000))
				// a random first byte must not decide the frame type
				if key {
					frame[0] = 0x86 // a VP9 inter frame header
				} else {
					frame[0] = 0x82 // a VP9 keyframe header
				}
				sent = append(sent, frame)
				payloads := packetize(ingestMTU, frame, &IngestFrame{Keyframe: key})
				require.NotEmpty(t, payloads)
				// the buffer guesses keyframes from the encrypted bitstream: wrong
				exts := extPackets(t, m, payloads, seq, ts, func(pl []byte) bool { return !key })
				got = append(got, assemble(t, a, m, exts, true)...)
				seq += uint64(len(payloads))
				ts += 3000
			}
			got = append(got, a.Tick(time.Now().Add(time.Second))...)
			require.Len(t, got, len(sent))
			for i, f := range got {
				require.Equal(t, sent[i], f.Data, "frame %d", i)
				require.Equal(t, i%10 == 0, f.Keyframe, "frame %d", i)
			}
		})
	}
}

// Encrypted AV1 carries the packet trailer after the payload, since the frame
// is not an OBU sequence that could hold a metadata OBU.
func TestEncryptedAV1Trailer(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	frame := randomBytes(r, 2500)
	meta := packettrailer.Metadata{HasFrameID: true, FrameID: 42}
	payloads := PacketizeAV1Opaque(ingestMTU, frame, &IngestFrame{Keyframe: true})
	last := len(payloads) - 1
	payloads[last] = packettrailer.AppendTrailer(append([]byte(nil), payloads[last]...), meta)

	a := NewAssembler(mime.MimeTypeAV1, AssemblerOptions{Encrypted: true})
	frames := assemble(t, a, mime.MimeTypeAV1, extPackets(t, mime.MimeTypeAV1, payloads, 1, 9000, func([]byte) bool { return false }), true)
	require.Len(t, frames, 1)
	require.Equal(t, frame, frames[0].Data)
	require.True(t, frames[0].Trailer.HasFrameID)
	require.Equal(t, uint32(42), frames[0].Trailer.FrameID)
}

func TestAV1ElementsWithLengths(t *testing.T) {
	// W=0: every element has a length; two elements
	payload := []byte{0x00, 2, 0xaa, 0xbb, 1, 0xcc}
	out, err := AppendAV1Elements(nil, &Packet{Payload: payload})
	require.NoError(t, err)
	require.Equal(t, []byte{0xaa, 0xbb, 0xcc}, out)
	// W=2: the second element runs to the end
	payload = []byte{0x20, 1, 0xaa, 0xbb, 0xcc}
	out, err = AppendAV1Elements(nil, &Packet{Payload: payload})
	require.NoError(t, err)
	require.Equal(t, []byte{0xaa, 0xbb, 0xcc}, out)
	// a length past the end
	_, err = AppendAV1Elements(nil, &Packet{Payload: []byte{0x00, 5, 1}})
	require.Error(t, err)
}

// vp9SVCPayload writes a flexible-mode VP9 descriptor with layer indices.
func vp9SVCPayload(pid uint16, sid uint8, interLayer, interPicture, first, last bool, data []byte) []byte {
	b0 := byte(0x80 | 0x20 | 0x10) // I, L, F
	if interPicture {
		b0 |= 0x40
	}
	if first {
		b0 |= 0x08
	}
	if last {
		b0 |= 0x04
	}
	l := sid << 1
	if interLayer {
		l |= 1
	}
	b := []byte{b0, 0x80 | byte(pid>>8), byte(pid), l}
	if interPicture {
		b = append(b, 1<<1)
	}
	return append(b, data...)
}

// An SVC picture is split by spatial layer; each spatial rendition gets the
// layers it decodes: all lower layers for full SVC and keyframes, only its
// own layer for a K-SVC delta picture.
func TestSVCSpatialRenditions(t *testing.T) {
	layer := func(sid uint8, n int) []byte { return bytes.Repeat([]byte{0x10 + sid}, n) }
	picture := func(pid uint16, key, kSVC bool, sizes []int) [][]byte {
		var out [][]byte
		for sid, n := range sizes {
			data := layer(uint8(sid), n)
			interLayer := sid > 0 && (key || !kSVC)
			for off := 0; off < len(data); off += 1000 {
				end := min(off+1000, len(data))
				out = append(out, vp9SVCPayload(pid, uint8(sid), interLayer, !key, off == 0, end == len(data), data[off:end]))
			}
		}
		return out
	}

	a := NewAssembler(mime.MimeTypeVP9, AssemblerOptions{})
	seq, ts := uint64(1), uint64(0)
	push := func(payloads [][]byte, key bool) []Frame {
		exts := extPackets(t, mime.MimeTypeVP9, payloads, seq, ts, func(pl []byte) bool {
			return key && pl[0]&0x08 != 0 && pl[3]>>1 == 0
		})
		seq += uint64(len(payloads))
		ts += 3000
		return assemble(t, a, mime.MimeTypeVP9, exts, false)
	}

	frames := push(picture(1, true, true, []int{300, 1200, 2500}), true)
	frames = append(frames, push(picture(2, false, true, []int{100, 400, 900}), false)...)
	frames = append(frames, push(picture(3, false, false, []int{100, 400, 900}), false)...) // full SVC delta
	frames = append(frames, push(picture(4, false, true, []int{100, 400}), false)...)       // top layer not sent
	frames = append(frames, a.Tick(time.Now().Add(time.Second))...)
	require.Len(t, frames, 4)

	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	key := frames[0]
	require.True(t, key.Keyframe)
	require.Len(t, key.Layers, 3)
	require.Equal(t, layer(0, 300), key.SpatialData(0))
	require.Equal(t, cat(layer(0, 300), layer(1, 1200)), key.SpatialData(1))
	require.Equal(t, cat(layer(0, 300), layer(1, 1200), layer(2, 2500)), key.SpatialData(2))
	require.Equal(t, key.Data, key.SpatialData(-1))

	kDelta := frames[1]
	require.False(t, kDelta.Keyframe)
	require.Equal(t, layer(0, 100), kDelta.SpatialData(0))
	require.Equal(t, layer(1, 400), kDelta.SpatialData(1), "K-SVC delta: no inter-layer dependency")
	require.Equal(t, layer(2, 900), kDelta.SpatialData(2))

	full := frames[2]
	require.Equal(t, cat(layer(0, 100), layer(1, 400)), full.SpatialData(1))
	require.Equal(t, full.Data, full.SpatialData(2))

	partial := frames[3]
	require.Equal(t, layer(1, 400), partial.SpatialData(2), "the highest layer present")
}

// H.265 from MoQ is packetized so the SFU buffer recognises keyframes and a
// WebRTC depacketizer restores the access unit.
func TestH265IngestPacketization(t *testing.T) {
	nal := func(typ byte, n int) []byte {
		b := []byte{0, 0, 0, 1, typ << 1, 1}
		for i := 0; i < n; i++ {
			b = append(b, byte(i%200)+1)
		}
		return b
	}
	au := bytes.Join([][]byte{nal(32, 20), nal(33, 40), nal(34, 8), nal(19, 5000)}, nil) // VPS SPS PPS IDR
	payloads := newPacketizer(mime.MimeTypeH265, false)(ingestMTU, au, true)
	require.Greater(t, len(payloads), 4)
	require.True(t, codec.IsH265KeyFrame(payloads[0]), "keyframe recognised on the first packet")

	a := NewAssembler(mime.MimeTypeH265, AssemblerOptions{})
	exts := extPackets(t, mime.MimeTypeH265, payloads, 10, 9000, codec.IsH265KeyFrame)
	frames := assemble(t, a, mime.MimeTypeH265, exts, false)
	frames = append(frames, a.Tick(time.Now().Add(time.Second))...)
	require.Len(t, frames, 1)
	require.True(t, frames[0].Keyframe)
	require.Equal(t, au, frames[0].Data)
}

// Opus DTX: during silence a publisher sends a packet every 400 ms; the RTP
// timestamps follow capture time across the gaps.
func TestOpusDTXTimestamps(t *testing.T) {
	l := &ingestLayer{isOpus: true}
	packet := []byte{0xfc, 1}
	ts := uint32(48000)
	require.Equal(t, ts, l.opusTimestamp(ts, packet))
	for i := 1; i <= 5; i++ {
		ts += 960
		require.Equal(t, ts, l.opusTimestamp(ts+uint32(i%3)*100, packet))
	}
	// DTX: 400 ms later, then again
	ts += 48 * 400
	require.Equal(t, ts, l.opusTimestamp(ts, packet))
	ts += 48 * 400
	require.Equal(t, ts, l.opusTimestamp(ts, packet))
	// speech resumes 20 ms later and stays on the packet timeline
	ts += 960
	require.Equal(t, ts, l.opusTimestamp(ts+200, packet))
}

// captureSender is a TrackSender recording what an IngestReceiver forwards.
type captureSender struct {
	mu   sync.Mutex
	pkts []capturedPacket
}

type capturedPacket struct {
	temporal int32
	keyframe bool
	vp8      *codec.VP8
	vp9      *codecs.VP9Packet
	marker   bool
}

func (c *captureSender) WriteRTP(p *buffer.ExtPacket, _ int32) int32 {
	cp := capturedPacket{temporal: p.VideoLayer.Temporal, keyframe: p.IsKeyFrame, marker: p.Packet.Marker}
	if p.VP8 != nil {
		v := *p.VP8
		cp.vp8 = &v
	}
	if p.VP9 != nil {
		v := *p.VP9
		v.PDiff = slices.Clone(v.PDiff)
		cp.vp9 = &v
	}
	c.mu.Lock()
	c.pkts = append(c.pkts, cp)
	c.mu.Unlock()
	return 1
}

func (c *captureSender) packets() []capturedPacket {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.pkts)
}

func (c *captureSender) UpTrackLayersChange()                       {}
func (c *captureSender) UpTrackBitrateAvailabilityChange()          {}
func (c *captureSender) UpTrackMaxPublishedLayerChange(int32)       {}
func (c *captureSender) UpTrackMaxTemporalLayerSeenChange(int32)    {}
func (c *captureSender) UpTrackBitrateReport([]int32, sfu.Bitrates) {}
func (c *captureSender) Close()                                     {}
func (c *captureSender) IsClosed() bool                             { return false }
func (c *captureSender) ID() string                                 { return "capture" }
func (c *captureSender) SubscriberID() livekit.ParticipantID        { return "PA_capture" }
func (c *captureSender) HandleRTCPSenderReportData(webrtc.PayloadType, int32, *livekit.RTCPSenderReportState) error {
	return nil
}
func (c *captureSender) Resync()                           {}
func (c *captureSender) SetReceiver(sfu.TrackReceiver)     {}
func (c *captureSender) ReceiverRestart(sfu.TrackReceiver) {}

// Temporal layer ids from LOC reach the SFU's RTP view (VP8 TID/Y, VP9
// TID/U/P_DIFF), so the forwarder can drop upper layers for a WebRTC
// subscriber and switch back up at sync frames.
func TestIngestTemporalLayers(t *testing.T) {
	pattern := []uint8{0, 2, 1, 2} // L1T3
	for _, m := range []mime.MimeType{mime.MimeTypeVP8, mime.MimeTypeVP9} {
		t.Run(m.String(), func(t *testing.T) {
			codecString := "vp8"
			if m == mime.MimeTypeVP9 {
				codecString = "vp09.00.10.08"
			}
			ti := &livekit.TrackInfo{Sid: "TR_temporal", Type: livekit.TrackType_VIDEO}
			r, err := NewIngestReceiver(IngestReceiverParams{
				TrackID:        "TR_temporal",
				StreamID:       "camera",
				Codec:          IngestCodec(m, codecString, 1),
				TrackInfo:      ti,
				NumLayers:      1,
				AudioConfig:    sfu.DefaultAudioConfig,
				StreamTrackers: sfu.DefaultStreamTrackerManagerConfig,
				Logger:         logger.GetLogger(),
			})
			require.NoError(t, err)
			defer r.Close()
			capture := &captureSender{}
			require.NoError(t, r.AddDownTrack(capture))

			start := uint64(time.Now().UnixMicro())
			const frames = 12
			for i := 0; i < frames; i++ {
				data := []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x40, 0x01, 0xf0, 0x00, byte(i)} // a VP8 keyframe header
				if m == mime.MimeTypeVP9 {
					data = vp9Keyframe(40)
				}
				if i > 0 {
					data = []byte{0x11, byte(i), 3, 4} // inter frame
					if m == mime.MimeTypeVP9 {
						data = []byte{0x86, byte(i), 3, 4}
					}
				}
				require.NoError(t, r.WriteFrame(0, IngestFrame{
					CaptureUs: start + uint64(i)*33_333, Keyframe: i == 0, Data: data,
					TemporalID: pattern[i%4], HasTemporalID: true,
				}))
			}
			var got []capturedPacket
			require.Eventually(t, func() bool { got = capture.packets(); return len(got) == frames }, 2*time.Second, 10*time.Millisecond)

			for i, p := range got {
				tid := pattern[i%4]
				require.Equal(t, int32(tid), p.temporal, "frame %d", i)
				// sync / switching-up frames reference the base layer only
				sync := i == 0 || (tid > 0 && (i%4 == 1 || i%4 == 2))
				if m == mime.MimeTypeVP8 {
					require.NotNil(t, p.vp8)
					require.Equal(t, tid, p.vp8.TID)
					require.Equal(t, sync, p.vp8.Y, "frame %d Y", i)
					require.True(t, p.vp8.L && p.vp8.T && p.vp8.M)
				} else {
					require.NotNil(t, p.vp9)
					require.Equal(t, tid, p.vp9.TID)
					if i > 0 {
						require.Equal(t, sync, p.vp9.U, "frame %d U", i)
						want := map[int]uint8{0: 4, 1: 1, 2: 2, 3: 1}[i%4]
						require.Equal(t, []uint8{want}, p.vp9.PDiff, "frame %d P_DIFF", i)
					}
				}
			}
			require.True(t, got[0].keyframe)

			if m == mime.MimeTypeVP8 {
				// the SFU's temporal selector: at layer 0 aiming for 2, it switches
				// up only at a sync frame of the next layer
				sel := temporallayerselector.NewVP8(logger.GetLogger())
				current := int32(0)
				var switched []int
				for i := 1; i < frames; i++ {
					ext := &buffer.ExtPacket{Packet: &rtp.Packet{Header: rtp.Header{Marker: true}}, VP8: got[i].vp8}
					ext.Temporal = got[i].temporal
					this, next := sel.Select(ext, current, 2)
					if this != current || next != current {
						switched = append(switched, i)
					}
					current = next
				}
				require.Equal(t, []int{2, 5}, switched, "up to TID 1 at frame 2, to TID 2 at frame 5")
			}
		})
	}
}
