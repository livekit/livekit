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
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/rtp/codecs/vp9"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/packettrailer"
)

var errShortPayload = errors.New("media: short payload")

const (
	ingestMTU          = 1188
	audioLevelExtID    = 1
	senderReportPeriod = time.Second
	ingestPayloadTypeA = 111
	ingestPayloadTypeV = 102
	maxIngestVideoPkts = 1 << 13
	maxIngestAudioPkts = 1 << 10
)

// IngestCodec returns the RTP codec parameters used for a MoQ-published track.
func IngestCodec(m mime.MimeType, codecString string, channels uint16) webrtc.RTPCodecParameters {
	c := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: m.String()},
		PayloadType:        ingestPayloadTypeV,
	}
	switch m {
	case mime.MimeTypeOpus:
		c.ClockRate, c.Channels = 48000, 2
		c.SDPFmtpLine = "minptime=10;useinbandfec=1"
		if channels == 2 {
			c.SDPFmtpLine += ";stereo=1"
		}
		c.PayloadType = ingestPayloadTypeA
	case mime.MimeTypePCMU, mime.MimeTypePCMA:
		c.ClockRate, c.Channels = 8000, 1
		c.PayloadType = 0
		if m == mime.MimeTypePCMA {
			c.PayloadType = 8
		}
	case mime.MimeTypeH264:
		c.ClockRate = 90000
		c.SDPFmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + H264ProfileLevelID(codecString)
	case mime.MimeTypeVP9:
		c.ClockRate = 90000
		c.SDPFmtpLine = "profile-id=" + vp9Profile(codecString)
	default:
		c.ClockRate = 90000
	}
	if mime.IsMimeTypeVideo(m) {
		c.RTCPFeedback = []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"}}
	}
	return c
}

// IngestFrame is one frame received over MoQ for a layer.
type IngestFrame struct {
	CaptureUs  uint64
	Keyframe   bool
	Data       []byte
	AudioLevel *AudioLevel
	Trailer    packettrailer.Metadata
	// TemporalID is the frame's temporal layer (LOC 0x3804) in a temporally
	// layered rendition; VP8 and VP9 carry it in their payload descriptors so
	// WebRTC subscribers can drop upper layers.
	TemporalID    uint8
	HasTemporalID bool
}

// IngestReceiverParams configures an IngestReceiver.
type IngestReceiverParams struct {
	TrackID     livekit.TrackID
	StreamID    string
	Codec       webrtc.RTPCodecParameters
	TrackInfo   *livekit.TrackInfo
	NumLayers   int
	AudioConfig sfu.AudioConfig
	// StreamTrackers and PLIThrottle should match WebRTC receivers'.
	StreamTrackers sfu.StreamTrackerManagerConfig
	PLIThrottle    sfu.PLIThrottleConfig
	Logger         logger.Logger
	// OnKeyframeRequest is called for PLIs from subscribers of a layer.
	OnKeyframeRequest func(layer int32)
}

// IngestReceiver is an sfu.TrackReceiver fed with frames received over MoQ.
// Each simulcast layer gets its own BufferBase and SSRC; frames are packetized
// into RTP so the rest of the SFU treats them like any published track.
type IngestReceiver struct {
	*sfu.ReceiverBase

	params IngestReceiverParams
	mime   mime.MimeType
	layers []*ingestLayer
}

type ingestLayer struct {
	mu        sync.Mutex
	layer     int32
	buff      *buffer.BufferBase
	ssrc      uint32
	packetize packetizeFunc
	seq       uint16
	rtpBase   uint32
	clockRate uint32
	baseUs    uint64
	started   bool
	lastTS    uint32
	lastSR    time.Time
	packets   uint32
	octets    uint64
	isAudio   bool
	waitKey   bool
	// opus follows the packets' own durations (nextTS) rather than jittery
	// capture times, re-anchoring after a gap
	isOpus bool
	haveTS bool
	nextTS uint32
	// av1 carries the trailer in a metadata OBU rather than after the frame
	// (unless encrypted: the frame is then not an OBU sequence)
	av1Trailer bool
}

func NewIngestReceiver(params IngestReceiverParams) (*IngestReceiver, error) {
	m := mime.NormalizeMimeType(params.Codec.MimeType)
	kind := webrtc.RTPCodecTypeVideo
	if mime.IsMimeTypeAudio(m) {
		kind = webrtc.RTPCodecTypeAudio
	}
	var exts []webrtc.RTPHeaderExtensionParameter
	if kind == webrtc.RTPCodecTypeAudio {
		exts = append(exts, webrtc.RTPHeaderExtensionParameter{URI: sdp.AudioLevelURI, ID: audioLevelExtID})
	}
	if params.NumLayers <= 0 {
		params.NumLayers = 1
	}
	encrypted := params.TrackInfo.GetEncryption() != livekit.Encryption_NONE
	r := &IngestReceiver{params: params, mime: m}
	r.ReceiverBase = sfu.NewReceiverBase(
		sfu.ReceiverBaseParams{
			TrackID:                    params.TrackID,
			StreamID:                   params.StreamID,
			Kind:                       kind,
			Codec:                      params.Codec,
			HeaderExtensions:           exts,
			Logger:                     params.Logger,
			StreamTrackerManagerConfig: params.StreamTrackers,
			IsSelfClosing:              false,
		},
		params.TrackInfo,
		sfu.ReceiverCodecStateNormal,
	)
	r.ReceiverBase.SetAudioConfig(params.AudioConfig)
	r.ReceiverBase.SetPLIThrottleConfig(params.PLIThrottle)

	rtpParams := webrtc.RTPParameters{
		Codecs:           []webrtc.RTPCodecParameters{params.Codec},
		HeaderExtensions: exts,
	}
	for i := 0; i < params.NumLayers; i++ {
		l := &ingestLayer{
			layer:      int32(i),
			ssrc:       randUint32(),
			seq:        uint16(randUint32()),
			rtpBase:    randUint32(),
			clockRate:  params.Codec.ClockRate,
			isAudio:    kind == webrtc.RTPCodecTypeAudio,
			isOpus:     m == mime.MimeTypeOpus,
			av1Trailer: m == mime.MimeTypeAV1 && !encrypted,
			waitKey:    kind == webrtc.RTPCodecTypeVideo,
			packetize:  newFramePacketizer(m, encrypted),
		}
		layer := int32(i)
		l.buff = buffer.NewBufferBase(buffer.BufferBaseParams{
			SSRC:         l.ssrc,
			MaxVideoPkts: maxIngestVideoPkts,
			MaxAudioPkts: maxIngestAudioPkts,
			SendPLI: func() {
				if params.OnKeyframeRequest != nil {
					params.OnKeyframeRequest(layer)
				}
			},
			IsReportingEnabled: true,
		})
		if params.Logger != nil {
			l.buff.SetLogger(params.Logger.WithValues("layer", i))
		}
		if err := l.buff.Bind(rtpParams, params.Codec.RTPCodecCapability, 0); err != nil {
			_, _ = l.buff.CloseWithReason("bind failed")
			r.Close()
			return nil, err
		}
		r.layers = append(r.layers, l)
		r.ReceiverBase.AddBuffer(l.buff, layer)
		r.ReceiverBase.StartBuffer(l.buff, layer)
	}
	return r, nil
}

// packetizeFunc splits a frame into RTP payloads.
type packetizeFunc func(mtu int, data []byte, f *IngestFrame) [][]byte

// temporalRefs infers a temporally layered stream's references from frame
// temporal ids, as the standard patterns (L1T2 0,1; L1T3 0,2,1,2) use them: a
// base layer frame references the previous base layer frame, an upper layer
// frame the most recent frame of a lower layer. Only frames of the same or
// lower layers are referenced, so a subscriber that drops upper layers never
// waits for them.
type temporalRefs struct {
	n      uint64
	latest [8]struct {
		n   uint64
		pid uint16
		ok  bool
	}
}

// next records a frame and returns the picture id of the frame it references
// and that frame's temporal id (ok false when there is none).
func (r *temporalRefs) next(pid uint16, tid uint8, keyframe bool) (refPID uint16, refTID uint8, ok bool) {
	tid &= 0x07
	r.n++
	if keyframe {
		for i := range r.latest {
			r.latest[i].ok = false
		}
	} else {
		var best uint64
		for t := uint8(0); t < 8; t++ {
			if (t < tid || t == 0 && tid == 0) && r.latest[t].ok && r.latest[t].n > best {
				best, refPID, refTID, ok = r.latest[t].n, r.latest[t].pid, t, true
			}
		}
	}
	r.latest[tid].n, r.latest[tid].pid, r.latest[tid].ok = r.n, pid, true
	return refPID, refTID, ok
}

// newPacketizer is newFramePacketizer for frames described by keyframe alone.
func newPacketizer(m mime.MimeType, encrypted bool) func(mtu int, data []byte, keyframe bool) [][]byte {
	p := newFramePacketizer(m, encrypted)
	return func(mtu int, data []byte, keyframe bool) [][]byte {
		return p(mtu, data, &IngestFrame{Keyframe: keyframe})
	}
}

// newFramePacketizer returns the RTP packetizer for a codec. Like
// libwebrtc's, the VP8 and VP9 packetizers take the payload descriptor from
// frame metadata (keyframe, temporal layer) and never parse the bitstream;
// encrypted AV1 frames, which are not OBU sequences, travel as opaque OBU
// elements.
func newFramePacketizer(m mime.MimeType, encrypted bool) packetizeFunc {
	var payloader rtp.Payloader
	switch m {
	case mime.MimeTypeVP8:
		p := &VP8Packetizer{PictureID: uint16(randUint32()) & 0x7fff, TL0PicIdx: uint8(randUint32())}
		return p.Packetize
	case mime.MimeTypeVP9:
		p := &VP9Packetizer{PictureID: uint16(randUint32()) & 0x7fff, Encrypted: encrypted}
		return p.Packetize
	case mime.MimeTypeH264:
		payloader = &codecs.H264Payloader{}
	case mime.MimeTypeH265:
		payloader = &codecs.H265Payloader{}
	case mime.MimeTypeAV1:
		if encrypted {
			return PacketizeAV1Opaque
		}
		payloader = &codecs.AV1Payloader{}
	case mime.MimeTypeOpus:
		payloader = &codecs.OpusPayloader{}
	default:
		payloader = &codecs.G711Payloader{}
	}
	return func(mtu int, data []byte, _ *IngestFrame) [][]byte {
		return payloader.Payload(uint16(mtu), data)
	}
}

// VP8Packetizer writes VP8 RTP payloads (RFC 7741) with a 15-bit picture id
// and, for temporally layered frames, TL0PICIDX and TID with the layer sync
// (Y) bit, which the SFU's temporal layer selection needs to switch up.
type VP8Packetizer struct {
	PictureID uint16
	TL0PicIdx uint8
	refs      temporalRefs
}

func (p *VP8Packetizer) Packetize(mtu int, data []byte, f *IngestFrame) [][]byte {
	if len(data) == 0 {
		return nil
	}
	pid := p.PictureID
	p.PictureID = (p.PictureID + 1) & 0x7fff
	hdr := []byte{0x80, 0x80, 0x80 | byte(pid>>8), byte(pid)} // X; I; M + picture id
	if f.HasTemporalID {
		tid := f.TemporalID & 0x03
		if tid == 0 {
			p.TL0PicIdx++
		}
		_, refTID, ok := p.refs.next(pid, tid, f.Keyframe)
		var tk byte = tid << 6
		if f.Keyframe || (tid > 0 && ok && refTID == 0) {
			tk |= 0x20 // Y: depends on base layer frames only
		}
		hdr[1] |= 0x40 | 0x20 // L, T
		hdr = append(hdr, p.TL0PicIdx, tk)
	}
	var out [][]byte
	for off := 0; off < len(data); {
		n := min(mtu-len(hdr), len(data)-off)
		if n <= 0 {
			return nil
		}
		pkt := make([]byte, 0, len(hdr)+n)
		pkt = append(pkt, hdr...)
		if off == 0 {
			pkt[0] |= 0x10 // S: start of partition 0
		}
		pkt = append(pkt, data[off:off+n]...)
		out = append(out, pkt)
		off += n
	}
	return out
}

// VP9Packetizer writes VP9 RTP payloads in flexible mode (RFC 9628) from frame
// metadata: picture id, inter-picture prediction (P, with P_DIFF 1) and, on
// keyframes, a scalability structure with the resolution when the frame is
// readable. libwebrtc receivers hand flexible-mode frames to the decoder
// without the GOF and TL0PICIDX that non-flexible mode requires, and the
// payload may be encrypted (E2EE encrypts VP9 from its first byte).
type VP9Packetizer struct {
	PictureID uint16
	Encrypted bool
	refs      temporalRefs
}

func (p *VP9Packetizer) Packetize(mtu int, data []byte, f *IngestFrame) [][]byte {
	if len(data) == 0 {
		return nil
	}
	keyframe := f.Keyframe
	var width, height uint16
	if keyframe && !p.Encrypted {
		var h vp9.Header
		if h.Unmarshal(data) == nil && !h.NonKeyFrame {
			width, height = h.Width(), h.Height()
		}
	}
	pid := p.PictureID
	p.PictureID = (p.PictureID + 1) & 0x7fff

	// temporally layered frames carry their layer (L) and reference the frame
	// their layer depends on (P_DIFF); others reference the previous picture
	var layerByte byte
	pdiff := uint16(1)
	if f.HasTemporalID {
		tid := f.TemporalID & 0x07
		refPID, refTID, ok := p.refs.next(pid, tid, keyframe)
		layerByte = tid << 5
		if ok {
			if d := (pid - refPID) & 0x7fff; d > 0 && d < 128 {
				pdiff = d
			}
			if tid > 0 && refTID == 0 {
				layerByte |= 0x10 // U: switching up point
			}
		}
	}

	header := func(first, last bool) []byte {
		b0 := byte(0x80 | 0x10) // I, F
		if !keyframe {
			b0 |= 0x40 // P
		}
		if first {
			b0 |= 0x08 // B
		}
		if last {
			b0 |= 0x04 // E
		}
		ss := keyframe && first
		if ss {
			b0 |= 0x02 // V
		}
		if f.HasTemporalID {
			b0 |= 0x20 // L
		}
		h := []byte{b0, 0x80 | byte(pid>>8), byte(pid)}
		if f.HasTemporalID {
			h = append(h, layerByte) // TID, U, SID 0, D 0
		}
		if !keyframe {
			h = append(h, byte(pdiff)<<1) // P_DIFF, N 0
		}
		if ss {
			if width > 0 && height > 0 {
				// N_S 0, Y 1, G 0, one layer's resolution
				h = append(h, 0x10, byte(width>>8), byte(width), byte(height>>8), byte(height))
			} else {
				h = append(h, 0x00)
			}
		}
		return h
	}

	var out [][]byte
	for off := 0; off < len(data); {
		first := off == 0
		h := header(first, false)
		n := min(mtu-len(h), len(data)-off)
		if n <= 0 {
			return nil
		}
		last := off+n == len(data)
		if last {
			h = header(first, true)
		}
		pkt := make([]byte, 0, len(h)+n)
		pkt = append(append(pkt, h...), data[off:off+n]...)
		out = append(out, pkt)
		off += n
	}
	return out
}

// PacketizeAV1Opaque writes an AV1 frame that is not an OBU sequence (E2EE
// encrypts AV1 from its first byte) as one OBU element fragmented across
// packets (Z/Y), N set on a keyframe's first packet. AppendAV1Elements
// restores the frame byte for byte.
func PacketizeAV1Opaque(mtu int, data []byte, f *IngestFrame) [][]byte {
	keyframe := f.Keyframe
	if len(data) == 0 || mtu < 2 {
		return nil
	}
	var out [][]byte
	for off := 0; off < len(data); {
		n := min(mtu-1, len(data)-off)
		agg := byte(1 << 4) // W 1: one element, without a length
		if off > 0 {
			agg |= 0x80 // Z: continues the previous packet's element
		}
		if off+n < len(data) {
			agg |= 0x40 // Y: continues in the next packet
		}
		if keyframe && off == 0 {
			agg |= 0x08 // N
		}
		pkt := make([]byte, 0, n+1)
		pkt = append(append(pkt, agg), data[off:off+n]...)
		out = append(out, pkt)
		off += n
	}
	return out
}

// WriteFrame packetizes a frame into the layer's buffer.
func (r *IngestReceiver) WriteFrame(layer int32, f IngestFrame) error {
	if int(layer) >= len(r.layers) || layer < 0 {
		return errors.New("media: invalid ingest layer")
	}
	l := r.layers[layer]
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.waitKey {
		if !f.Keyframe {
			return nil
		}
		l.waitKey = false
	}
	if !l.started {
		l.started = true
		l.baseUs = f.CaptureUs
	}
	var deltaUs uint64
	if f.CaptureUs > l.baseUs {
		deltaUs = f.CaptureUs - l.baseUs
	}
	ts := l.rtpBase + uint32(deltaUs*uint64(l.clockRate)/1_000_000)
	if l.isOpus {
		ts = l.opusTimestamp(ts, f.Data)
	}
	l.lastTS = ts

	data := f.Data
	if l.av1Trailer && !f.Trailer.IsEmpty() {
		data = packettrailer.InsertAV1(data, f.Trailer)
	}
	payloads := l.packetize(ingestMTU, data, &f)
	if len(payloads) == 0 {
		return nil
	}
	if !l.av1Trailer && !f.Trailer.IsEmpty() {
		last := len(payloads) - 1
		payloads[last] = packettrailer.AppendTrailer(append([]byte(nil), payloads[last]...), f.Trailer)
	}
	now := mono.UnixNano()
	for i, payload := range payloads {
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    uint8(r.params.Codec.PayloadType),
				SequenceNumber: l.seq,
				Timestamp:      ts,
				SSRC:           l.ssrc,
				Marker:         i == len(payloads)-1,
			},
			Payload: payload,
		}
		if l.isAudio {
			// audio frames are single packets: marker only on talk spurt start is
			// what senders do, but the SFU does not depend on it
			pkt.Marker = false
			if f.AudioLevel != nil {
				v := f.AudioLevel.Level & 0x7f
				if f.AudioLevel.Voice {
					v |= 0x80
				}
				_ = pkt.SetExtension(audioLevelExtID, []byte{v})
			}
		}
		l.seq++
		raw, err := pkt.Marshal()
		if err != nil {
			return err
		}
		if _, _, err := l.buff.HandleIncomingPacket(raw, nil, now, false, false, nil, 0); err != nil {
			return err
		}
		l.packets++
		l.octets += uint64(len(payload))
	}

	// sender reports map RTP time to the publisher's capture clock so A/V sync
	// and simulcast layer switching work for WebRTC subscribers
	if wall := time.Now(); wall.Sub(l.lastSR) >= senderReportPeriod {
		l.lastSR = wall
		captureTime := time.UnixMicro(int64(f.CaptureUs))
		l.buff.SetSenderReportData(&livekit.RTCPSenderReportState{
			RtpTimestamp: ts,
			NtpTimestamp: uint64(mediatransportutil.ToNtpTime(captureTime)),
			Packets:      l.packets,
			Octets:       l.octets,
			At:           mono.UnixNano(),
		})
	}
	return nil
}

// SkipUntilKeyframe drops frames on a layer until the next keyframe, e.g.
// after a gap in the MoQ subscription.
func (r *IngestReceiver) SkipUntilKeyframe(layer int32) {
	if int(layer) >= len(r.layers) || layer < 0 || r.layers[layer].isAudio {
		return
	}
	l := r.layers[layer]
	l.mu.Lock()
	l.waitKey = true
	l.mu.Unlock()
}

// Close closes the receiver and its buffers.
func (r *IngestReceiver) Close() {
	r.ReceiverBase.Close("moq-ingest-closed", true)
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	v := binary.BigEndian.Uint32(b[:])
	if v == 0 {
		v = 1
	}
	return v
}

var _ sfu.TrackReceiver = (*IngestReceiver)(nil)

// vp9Profile is the profile of a "vp09.PP.LL.DD" codec string ("0" if absent).
func vp9Profile(codecString string) string {
	parts := strings.Split(codecString, ".")
	if len(parts) > 1 {
		if p, err := strconv.Atoi(parts[1]); err == nil && p >= 0 && p <= 3 {
			return strconv.Itoa(p)
		}
	}
	return "0"
}

// opusResyncSamples is how far (80 ms) capture times may drift from the
// packet-duration timeline before it re-anchors on them.
const opusResyncSamples = 80 * 48

// opusTimestamp returns the RTP timestamp of an Opus packet: the previous
// packet's timestamp plus its duration while the capture-derived timestamp
// stays close, so publisher scheduling jitter does not reach the receivers'
// jitter buffers; the capture time after a gap (mute, DTX, stall).
func (l *ingestLayer) opusTimestamp(captureTS uint32, packet []byte) uint32 {
	ts := captureTS
	if l.haveTS {
		if d := int32(captureTS - l.nextTS); d > -opusResyncSamples && d < opusResyncSamples {
			ts = l.nextTS
		}
	}
	if n := opusPacketSamples(packet); n > 0 {
		l.nextTS, l.haveTS = ts+uint32(n), true
	} else {
		l.haveTS = false
	}
	return ts
}

// opusPacketSamples is the duration of an Opus packet in 48 kHz samples, from
// its TOC byte (RFC 6716 section 3.1); 0 if malformed.
func opusPacketSamples(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	config := b[0] >> 3
	var frame int // in 1/10 ms
	switch {
	case config < 12: // SILK: 10, 20, 40, 60 ms
		frame = []int{100, 200, 400, 600}[config%4]
	case config < 16: // hybrid: 10, 20 ms
		frame = []int{100, 200}[config%2]
	default: // CELT: 2.5, 5, 10, 20 ms
		frame = []int{25, 50, 100, 200}[config%4]
	}
	frames := 1
	switch b[0] & 0x03 {
	case 1, 2:
		frames = 2
	case 3:
		if len(b) < 2 {
			return 0
		}
		frames = int(b[1] & 0x3f)
	}
	return frames * frame * 48 / 10
}
