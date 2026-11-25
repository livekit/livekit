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
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/codec"

	"github.com/livekit/protocol/codecs/mime"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
)

// h264Frame builds an Annex-B access unit with SPS, PPS and an IDR large
// enough to need FU-A fragmentation.
func h264Frame(idr bool, size int) []byte {
	var b []byte
	if idr {
		b = append(b, 0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0x8d)
		b = append(b, 0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80)
		b = append(b, 0, 0, 0, 1, 0x65)
	} else {
		b = append(b, 0, 0, 0, 1, 0x41)
	}
	for i := 0; i < size; i++ {
		b = append(b, byte(i%251)+1)
	}
	return b
}

func h264Packets(t *testing.T, frame []byte, seq uint64, ts uint64, key bool) []Packet {
	var p codecs.H264Payloader
	payloads := p.Payload(1200, frame)
	var out []Packet
	for i, pl := range payloads {
		pk := Packet{ExtSeq: seq + uint64(i), ExtTS: ts, Marker: i == len(payloads)-1, Payload: pl, Keyframe: key && i == 0, TemporalID: -1, SpatialID: -1}
		switch pl[0] & 0x1f {
		case 24:
			v := true
			pk.FrameStart = &v
		case 28:
			if pl[1]&0x80 == 0 {
				v := false
				pk.FrameStart = &v
			}
		}
		out = append(out, pk)
	}
	return out
}

func stripStartCodes(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{0, 0, 0, 1}, nil)
}

func TestAssemblerH264ReorderAndLoss(t *testing.T) {
	losses := 0
	a := NewAssembler(mime.MimeTypeH264, AssemblerOptions{MaxWait: 50 * time.Millisecond, OnLoss: func() { losses++ }})
	now := time.Now()

	f1 := h264Frame(true, 3000)
	p1 := h264Packets(t, f1, 100, 9000, true)
	require.Greater(t, len(p1), 2)
	// deliver out of order: last packet first
	var frames []Frame
	frames = append(frames, a.Push(p1[len(p1)-1], now)...)
	for _, p := range p1[:len(p1)-1] {
		frames = append(frames, a.Push(p, now)...)
	}
	require.Len(t, frames, 1)
	require.True(t, frames[0].Keyframe)
	require.Equal(t, stripStartCodes(f1), stripStartCodes(frames[0].Data))

	// delta frame with a lost middle packet: no output until the gap times out
	f2 := h264Frame(false, 3000)
	p2 := h264Packets(t, f2, p1[len(p1)-1].ExtSeq+1, 12000, false)
	for i, p := range p2 {
		if i == 1 {
			continue
		}
		require.Empty(t, a.Push(p, now))
	}
	// next delta frame arrives, gap still open
	f3 := h264Frame(false, 100)
	p3 := h264Packets(t, f3, p2[len(p2)-1].ExtSeq+1, 15000, false)
	require.Empty(t, a.Push(p3[0], now))
	// after MaxWait the loss is declared, and deltas are dropped until a keyframe
	require.Empty(t, a.Tick(now.Add(100*time.Millisecond)))
	require.Equal(t, 1, losses)

	f4 := h264Frame(true, 500)
	p4 := h264Packets(t, f4, p3[len(p3)-1].ExtSeq+1, 18000, true)
	frames = nil
	for _, p := range p4 {
		frames = append(frames, a.Push(p, now.Add(110*time.Millisecond))...)
	}
	require.Len(t, frames, 1)
	require.True(t, frames[0].Keyframe)
	require.True(t, frames[0].AfterLoss)
}

func TestAssemblerAudioSkipsGaps(t *testing.T) {
	a := NewAssembler(mime.MimeTypeOpus, AssemblerOptions{MaxWait: 20 * time.Millisecond})
	now := time.Now()
	require.Len(t, a.Push(Packet{ExtSeq: 1, ExtTS: 960, Payload: []byte{1}}, now), 1)
	// seq 2 lost
	require.Empty(t, a.Push(Packet{ExtSeq: 3, ExtTS: 2880, Payload: []byte{3}}, now))
	got := a.Tick(now.Add(30 * time.Millisecond))
	require.Len(t, got, 1)
	require.Equal(t, uint64(2880), got[0].ExtTS)
}

func TestCatalogRoundTrip(t *testing.T) {
	c := &Catalog{Tracks: []CatalogTrack{{
		Name: "TR_x/h264/f", Packaging: PackagingLOC, IsLive: true, Role: RoleVideo,
		Codec: WebCodecsString(mime.MimeTypeH264, "profile-level-id=42e01f"), Width: 1280, Height: 720,
		LiveKit: &TrackExt{Sid: "TR_x", PublisherSID: "PA_x"},
	}}}
	b, err := c.Marshal()
	require.NoError(t, err)
	require.Contains(t, string(b), `"version":"draft-02"`)
	require.Contains(t, string(b), `"codec":"avc3.42e01f"`)
	got, err := ParseCatalog(b)
	require.NoError(t, err)
	require.Equal(t, "TR_x", got.Tracks[0].LiveKit.Sid)
	m, ok := MimeFromCodecString(got.Tracks[0].Codec)
	require.True(t, ok)
	require.Equal(t, mime.MimeTypeH264, m)
}

func TestCatalogCodecParameters(t *testing.T) {
	require.Equal(t, "hev1.1.2.L93.B0", WebCodecsString(mime.MimeTypeH265, ""))
	require.Equal(t, "hev1.A2.6.H120.90.01", WebCodecsString(mime.MimeTypeH265,
		"profile-space=1;profile-id=2;tier-flag=1;level-id=120;profile-compatibility-indicator=60000000;interop-constraints=900100000000"))
	require.Equal(t, "vp09.01.00.08", WebCodecsString(mime.MimeTypeVP9, "profile-id=1"))
	require.Equal(t, "av01.0.05M.08", WebCodecsString(mime.MimeTypeAV1, ""))
	require.Equal(t, "av01.1.09H.08", WebCodecsString(mime.MimeTypeAV1, "profile=1;level-idx=9;tier=1"))
}

// A VP8 publisher that negotiated the dependency descriptor (libwebrtc
// simulcast) still carries the VP8 payload descriptor, which must be stripped.
func TestVP8DescriptorStrippedWithDependencyDescriptor(t *testing.T) {
	frame := []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x40, 0x01, 0xf0, 0x00, 0xaa}
	payload := append([]byte{0x90, 0x80, 0x05}, frame...) // X S, I, 7-bit picture id
	var vp8 codec.VP8
	require.NoError(t, vp8.Unmarshal(payload))
	ext := &buffer.ExtPacket{
		Packet:               &rtp.Packet{Header: rtp.Header{Marker: true}, Payload: payload},
		VP8:                  &vp8,
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{Descriptor: &dd.DependencyDescriptor{FirstPacketInFrame: true, LastPacketInFrame: true}},
		IsKeyFrame:           true,
		ExtSequenceNumber:    100,
		ExtTimestamp:         9000,
	}
	ext.VideoLayer.Spatial, ext.VideoLayer.Temporal = 0, 0
	a := NewAssembler(mime.MimeTypeVP8, AssemblerOptions{})
	frames := a.Push(PacketFromExt(ext, mime.MimeTypeVP8, nil, false), time.Now())
	frames = append(frames, a.Tick(time.Now().Add(time.Second))...)
	require.Len(t, frames, 1)
	require.Equal(t, frame, frames[0].Data)
	require.True(t, frames[0].Keyframe)
}

func TestOpusTimestampsFollowPacketDurations(t *testing.T) {
	require.Equal(t, 960, opusPacketSamples([]byte{0xfc}))      // CELT 20 ms
	require.Equal(t, 480, opusPacketSamples([]byte{0x00}))      // SILK 10 ms
	require.Equal(t, 960, opusPacketSamples([]byte{0x78}))      // hybrid 20 ms
	require.Equal(t, 2*960, opusPacketSamples([]byte{0xfd}))    // two 20 ms frames
	require.Equal(t, 3*960, opusPacketSamples([]byte{0xff, 3})) // code 3, three frames
	require.Equal(t, 0, opusPacketSamples(nil))

	l := &ingestLayer{isOpus: true}
	packet := []byte{0xfc, 1, 2, 3}
	ts := l.opusTimestamp(1000, packet)
	require.Equal(t, uint32(1000), ts)
	// jittery capture times snap to the 960-sample timeline
	require.Equal(t, uint32(1960), l.opusTimestamp(1960+300, packet))
	require.Equal(t, uint32(2920), l.opusTimestamp(2920-500, packet))
	// a gap longer than 80 ms re-anchors on the capture time
	require.Equal(t, uint32(2920+960+48*200), l.opusTimestamp(2920+960+48*200, packet))
}
