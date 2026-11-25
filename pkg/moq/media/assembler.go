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
	"slices"
	"time"

	"github.com/pion/rtp/codecs"

	"github.com/livekit/protocol/codecs/mime"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/packettrailer"
)

// Packet is the subset of an ExtPacket the assembler keeps. Payload is copied
// because ExtPacket payloads are reused once the forwarder moves on.
type Packet struct {
	ExtSeq     uint64
	ExtTS      uint64
	Marker     bool
	Payload    []byte
	Keyframe   bool
	TemporalID int8
	SpatialID  int8
	// FrameStart is a codec-level start-of-frame hint (nil when unknown).
	FrameStart *bool
	// InterLayer is set when the packet's spatial layer frame depends on lower
	// spatial layers of the same picture (VP9 D bit; assumed when unknown).
	InterLayer bool
	AudioLevel *AudioLevel
	Arrival    int64
	// vp8Header is the VP8 payload descriptor length to strip.
	vp8Header int
}

// AudioLevel is an RFC 6464 level (0 loudest .. 127 silent) and voice flag.
type AudioLevel struct {
	Level uint8
	Voice bool
}

// PacketFromExt copies what the assembler needs out of an ExtPacket.
//
// encrypted is set for end-to-end encrypted tracks. libwebrtc's frame
// cryptor encrypts VP9 and AV1 frames from their first byte, so keyframes are
// then recognised from the RTP payload descriptor (VP9 P bit, AV1 N bit),
// which the sender fills from encoder metadata, instead of the bitstream.
func PacketFromExt(ext *buffer.ExtPacket, m mime.MimeType, audioLevel *AudioLevel, encrypted bool) Packet {
	p := Packet{
		ExtSeq:     ext.ExtSequenceNumber,
		ExtTS:      ext.ExtTimestamp,
		Marker:     ext.Packet.Marker,
		Payload:    slices.Clone(ext.Packet.Payload),
		Keyframe:   ext.IsKeyFrame,
		TemporalID: -1,
		SpatialID:  -1,
		InterLayer: true,
		AudioLevel: audioLevel,
		Arrival:    ext.Arrival,
	}
	if ext.VideoLayer.Temporal >= 0 {
		p.TemporalID = int8(ext.VideoLayer.Temporal)
	}
	if ext.VideoLayer.Spatial >= 0 {
		p.SpatialID = int8(ext.VideoLayer.Spatial)
	}
	start := func(v bool) *bool { return &v }
	if m == mime.MimeTypeVP8 && ext.VP8 != nil {
		// the payload descriptor is stripped whichever header marks frame starts
		p.vp8Header = ext.VP8.HeaderSize
	}
	switch {
	case ext.DependencyDescriptor != nil && ext.DependencyDescriptor.Descriptor != nil:
		d := ext.DependencyDescriptor.Descriptor
		p.FrameStart = start(d.FirstPacketInFrame && (p.SpatialID <= 0))
	case m == mime.MimeTypeVP8 && ext.VP8 != nil:
		p.FrameStart = start(ext.VP8.S && ext.VP8.FirstByte&0x07 == 0)
		if ext.VP8.T {
			p.TemporalID = int8(ext.VP8.TID)
		}
	case m == mime.MimeTypeVP9 && ext.VP9 != nil:
		p.FrameStart = start(ext.VP9.B && ext.VP9.SID == 0)
		p.InterLayer = ext.VP9.D
	case m == mime.MimeTypeH264 && len(p.Payload) > 1:
		switch p.Payload[0] & 0x1f {
		case 24, 7, 9: // STAP-A, SPS, AUD begin an access unit
			p.FrameStart = start(true)
		case 28: // FU-A
			if p.Payload[1]&0x80 == 0 {
				p.FrameStart = start(false)
			}
		}
	case m == mime.MimeTypeH265 && len(p.Payload) > 2:
		switch (p.Payload[0] >> 1) & 0x3f {
		case 48, 32, 33, 35: // AP, VPS, SPS, AUD
			p.FrameStart = start(true)
		case 49: // FU
			if p.Payload[2]&0x80 == 0 {
				p.FrameStart = start(false)
			}
		}
	case m == mime.MimeTypeAV1 && ext.IsKeyFrame:
		// the N bit (new coded video sequence) is only set on a keyframe's first packet
		p.FrameStart = start(true)
	case mime.IsMimeTypeAudio(m):
		p.FrameStart = start(true)
	}
	if encrypted {
		switch m {
		case mime.MimeTypeVP9:
			vp9 := ext.VP9
			if vp9 == nil {
				vp9 = &codecs.VP9Packet{}
				if _, err := vp9.Unmarshal(ext.Packet.Payload); err != nil {
					vp9 = nil
				}
			}
			if vp9 != nil {
				p.Keyframe = !vp9.P && vp9.SID == 0
				if p.FrameStart == nil {
					p.FrameStart = start(vp9.B && vp9.SID == 0)
				}
			}
		case mime.MimeTypeAV1:
			// N: first packet of a coded video sequence, set on keyframes
			p.Keyframe = len(p.Payload) > 0 && p.Payload[0]&0x88 == 0x08
			if p.Keyframe {
				p.FrameStart = start(true)
			}
		}
	}
	return p
}

// Frame is an assembled, depacketized media frame.
type Frame struct {
	Data       []byte
	ExtTS      uint64
	Keyframe   bool
	TemporalID int8
	SpatialID  int8
	Arrival    int64
	Trailer    packettrailer.Metadata
	AudioLevel *AudioLevel
	// AfterLoss marks the first frame after unrecoverable loss.
	AfterLoss bool
	// Layers splits Data by spatial layer for an SVC picture (VP9/AV1 with
	// several spatial layers); nil otherwise. Spans are in ascending spatial
	// order and alias Data.
	Layers []LayerSpan
}

// LayerSpan is one spatial layer's part of an SVC picture.
type LayerSpan struct {
	Spatial int8
	Data    []byte
	// InterLayer: this layer frame depends on the lower layers of the picture.
	InterLayer bool
}

// SpatialData returns the bitstream that decodes the picture at spatial
// layer maxSpatial: that layer's frame (or the highest one below it present
// in the picture) and the lower layers it depends on. In K-SVC (inter-layer
// prediction on keyframes only) a delta picture then carries only the
// selected layer, as a WebRTC SFU forwards it. A picture without layers is
// returned whole.
func (f *Frame) SpatialData(maxSpatial int8) []byte {
	if len(f.Layers) == 0 || maxSpatial < 0 {
		return f.Data
	}
	top := -1
	for i, l := range f.Layers {
		if l.Spatial <= maxSpatial {
			top = i
		}
	}
	if top < 0 {
		return nil
	}
	if top == len(f.Layers)-1 && (f.Keyframe || f.allDepend(top)) {
		return f.Data
	}
	first := top
	if f.Keyframe {
		// a group starts here: the rendition must decode from this picture
		// alone, so it carries every lower layer
		first = 0
	}
	for first > 0 && f.Layers[first].InterLayer {
		first--
	}
	if first == top {
		return f.Layers[top].Data
	}
	var out []byte
	for _, l := range f.Layers[first : top+1] {
		out = append(out, l.Data...)
	}
	return out
}

// allDepend reports whether every layer up to top depends on the one below,
// so the whole picture up to top is needed.
func (f *Frame) allDepend(top int) bool {
	for i := 1; i <= top; i++ {
		if !f.Layers[i].InterLayer {
			return false
		}
	}
	return true
}

// AssemblerOptions tunes loss handling.
type AssemblerOptions struct {
	// MaxWait is how long a sequence gap may stay open before it is declared lost.
	MaxWait time.Duration
	// MaxPackets bounds buffered packets.
	MaxPackets int
	// OnLoss is called when frames are lost and a keyframe is needed.
	OnLoss func()
	// Encrypted is set for end-to-end encrypted tracks: an AV1 frame is then
	// the concatenation of its RTP OBU elements as sent, since the payload is
	// not an OBU sequence the depacketizer may rewrite, and its packet trailer
	// follows the payload as for other codecs.
	Encrypted bool
}

// Assembler turns an RTP packet stream for one layer into frames. It tolerates
// reordering and waits up to MaxWait for retransmissions; after unrecoverable
// loss it drops frames until the next keyframe (video).
type Assembler struct {
	mime    mime.MimeType
	isAudio bool
	opts    AssemblerOptions

	pkts         map[uint64]*Packet
	next         uint64
	started      bool
	gapSince     time.Time
	needKeyframe bool
	afterLoss    bool
}

func NewAssembler(m mime.MimeType, opts AssemblerOptions) *Assembler {
	isAudio := mime.IsMimeTypeAudio(m)
	if opts.MaxWait <= 0 {
		if isAudio {
			opts.MaxWait = 80 * time.Millisecond
		} else {
			opts.MaxWait = 300 * time.Millisecond
		}
	}
	if opts.MaxPackets <= 0 {
		opts.MaxPackets = 4096
	}
	return &Assembler{mime: m, isAudio: isAudio, opts: opts, pkts: make(map[uint64]*Packet), needKeyframe: !isAudio}
}

// Push adds a packet and returns frames that became complete, in order.
func (a *Assembler) Push(p Packet, now time.Time) []Frame {
	if a.started && p.ExtSeq < a.next {
		return nil // late duplicate or retransmission of something already handled
	}
	if _, ok := a.pkts[p.ExtSeq]; ok {
		return nil
	}
	pp := p
	a.pkts[p.ExtSeq] = &pp
	if len(a.pkts) > a.opts.MaxPackets {
		a.reset()
		return nil
	}
	if !a.started && !a.findStart() {
		return nil
	}
	return a.drain(now)
}

// findStart picks the first buffered packet that can begin decoding.
func (a *Assembler) findStart() bool {
	seqs := make([]uint64, 0, len(a.pkts))
	for s := range a.pkts {
		seqs = append(seqs, s)
	}
	slices.Sort(seqs)
	for i, s := range seqs {
		p := a.pkts[s]
		if !a.isStart(p) {
			continue
		}
		if p.FrameStart == nil && !a.isAudio {
			// unknown codec hint: only trust it after a timestamp change
			prev, ok := a.pkts[s-1]
			if !ok || prev.ExtTS == p.ExtTS {
				continue
			}
		}
		for _, d := range seqs[:i] {
			delete(a.pkts, d)
		}
		a.started = true
		a.next = s
		return true
	}
	return false
}

// Tick lets the assembler declare loss when no packets arrive.
func (a *Assembler) Tick(now time.Time) []Frame {
	if !a.started {
		return nil
	}
	return a.drain(now)
}

func (a *Assembler) isStart(p *Packet) bool {
	if a.isAudio {
		return true
	}
	if a.needKeyframe && !p.Keyframe {
		return false
	}
	return p.FrameStart == nil || *p.FrameStart
}

func (a *Assembler) reset() {
	a.pkts = make(map[uint64]*Packet)
	a.started = false
	a.gapSince = time.Time{}
	if !a.isAudio {
		a.needKeyframe = true
		a.afterLoss = true
		if a.opts.OnLoss != nil {
			a.opts.OnLoss()
		}
	}
}

func (a *Assembler) drain(now time.Time) []Frame {
	var out []Frame
	for {
		end, ok := a.findFrameEnd()
		if ok {
			a.gapSince = time.Time{}
			if f, emit := a.takeFrame(end); emit {
				out = append(out, f)
			}
			continue
		}
		if len(a.pkts) == 0 {
			return out
		}
		// a gap blocks progress
		if a.gapSince.IsZero() {
			a.gapSince = now
			return out
		}
		if now.Sub(a.gapSince) < a.opts.MaxWait {
			return out
		}
		a.gapSince = time.Time{}
		if !a.resync() {
			return out
		}
	}
}

// findFrameEnd looks for a complete frame starting at a.next.
func (a *Assembler) findFrameEnd() (uint64, bool) {
	head, ok := a.pkts[a.next]
	if !ok {
		return 0, false
	}
	if a.isAudio {
		return a.next, true
	}
	for s := a.next; ; s++ {
		p, ok := a.pkts[s]
		if !ok {
			return 0, false
		}
		if p.ExtTS != head.ExtTS {
			return s - 1, true // timestamp change closes the frame even without a marker
		}
		if p.Marker {
			return s, true
		}
	}
}

func (a *Assembler) takeFrame(end uint64) (Frame, bool) {
	pkts := make([]*Packet, 0, end-a.next+1)
	for s := a.next; s <= end; s++ {
		pkts = append(pkts, a.pkts[s])
		delete(a.pkts, s)
	}
	a.next = end + 1

	f := Frame{
		ExtTS:      pkts[0].ExtTS,
		TemporalID: pkts[0].TemporalID,
		SpatialID:  pkts[0].SpatialID,
		Arrival:    pkts[len(pkts)-1].Arrival,
		AudioLevel: pkts[0].AudioLevel,
	}
	for _, p := range pkts {
		if p.Keyframe {
			f.Keyframe = true
		}
	}
	if !a.isAudio && a.needKeyframe {
		if !f.Keyframe {
			return Frame{}, false
		}
		a.needKeyframe = false
	}

	// AV1 carries the trailer in a metadata OBU, other codecs (and encrypted
	// AV1) after the frame
	obuTrailer := a.mime == mime.MimeTypeAV1 && !a.opts.Encrypted
	last := pkts[len(pkts)-1]
	if !obuTrailer {
		if meta, n := packettrailer.ParseTrailer(last.Payload, last.Marker || a.isAudio); n > 0 &&
			packettrailer.ShrinkLastAggregatedNALU(last.Payload, a.mime, n) {
			f.Trailer = meta
			last.Payload = last.Payload[:len(last.Payload)-n]
		}
	}
	data, spans, err := depacketize(a.mime, pkts, a.opts.Encrypted)
	if err != nil || len(data) == 0 {
		if !a.isAudio {
			a.needKeyframe = true
			a.afterLoss = true
			if a.opts.OnLoss != nil {
				a.opts.OnLoss()
			}
		}
		return Frame{}, false
	}
	if obuTrailer {
		if len(spans) > 1 {
			data, spans = extractAV1TrailerFromSpans(&f, spans)
		} else if meta, stripped, ok := packettrailer.ExtractAV1(data); ok {
			f.Trailer, data = meta, stripped
		}
	}
	f.Data = data
	if len(spans) > 1 {
		f.Layers = spans
	}
	f.AfterLoss = a.afterLoss
	a.afterLoss = false
	return f, true
}

// resync skips a gap that will not be repaired.
func (a *Assembler) resync() bool {
	if len(a.pkts) == 0 {
		return false
	}
	seqs := make([]uint64, 0, len(a.pkts))
	for s := range a.pkts {
		seqs = append(seqs, s)
	}
	slices.Sort(seqs)
	if a.isAudio {
		a.next = seqs[0]
		return true
	}
	a.needKeyframe = true
	a.afterLoss = true
	if a.opts.OnLoss != nil {
		a.opts.OnLoss()
	}
	// restart at the first buffered keyframe start, dropping everything before it
	for i, s := range seqs {
		p := a.pkts[s]
		prev, hasPrev := a.pkts[s-1]
		isStart := p.FrameStart != nil && *p.FrameStart
		if p.FrameStart == nil && hasPrev && prev.ExtTS != p.ExtTS {
			isStart = true
		}
		if p.Keyframe && isStart {
			for _, d := range seqs[:i] {
				delete(a.pkts, d)
			}
			a.next = s
			return true
		}
	}
	// nothing usable yet: drop it all and wait for a keyframe start
	a.pkts = make(map[uint64]*Packet)
	a.started = false
	return false
}

// depacketize converts a frame's RTP payloads into the codec bitstream. For
// a picture with several spatial layers it also returns the spans of each
// layer (packets carry one layer frame each, in ascending layer order).
func depacketize(m mime.MimeType, pkts []*Packet, encrypted bool) ([]byte, []LayerSpan, error) {
	size := 0
	for _, p := range pkts {
		size += len(p.Payload)
	}
	out := make([]byte, 0, size)

	type mark struct {
		spatial    int8
		start      int
		interLayer bool
	}
	var marks []mark
	layered := false
	for _, p := range pkts {
		if p.SpatialID > 0 {
			layered = true
			break
		}
	}
	unmarshal := depacketizerFor(m, encrypted)
	for _, p := range pkts {
		if layered && (len(marks) == 0 || marks[len(marks)-1].spatial != p.SpatialID) {
			marks = append(marks, mark{spatial: p.SpatialID, start: len(out), interLayer: p.InterLayer})
		}
		var err error
		if out, err = unmarshal(out, p); err != nil {
			return nil, nil, err
		}
	}
	if len(marks) < 2 {
		return out, nil, nil
	}
	spans := make([]LayerSpan, 0, len(marks))
	for i, mk := range marks {
		end := len(out)
		if i+1 < len(marks) {
			end = marks[i+1].start
		}
		if end > mk.start {
			spans = append(spans, LayerSpan{Spatial: mk.spatial, Data: out[mk.start:end:end], InterLayer: mk.interLayer})
		}
	}
	slices.SortStableFunc(spans, func(a, b LayerSpan) int { return int(a.Spatial) - int(b.Spatial) })
	return out, spans, nil
}

// depacketizerFor returns a function appending a packet's bitstream to out.
// Stateful depacketizers (H264 FU-A, H265 FU, AV1 fragments) live for one
// frame.
func depacketizerFor(m mime.MimeType, encrypted bool) func(out []byte, p *Packet) ([]byte, error) {
	switch m {
	case mime.MimeTypeVP8:
		return func(out []byte, p *Packet) ([]byte, error) {
			if p.vp8Header > len(p.Payload) {
				return nil, errShortPayload
			}
			return append(out, p.Payload[p.vp8Header:]...), nil
		}
	case mime.MimeTypeVP9:
		return func(out []byte, p *Packet) ([]byte, error) {
			var vp9 codecs.VP9Packet
			b, err := vp9.Unmarshal(p.Payload)
			if err != nil {
				return nil, err
			}
			return append(out, b...), nil
		}
	case mime.MimeTypeH264:
		var h264 codecs.H264Packet
		return func(out []byte, p *Packet) ([]byte, error) {
			b, err := h264.Unmarshal(p.Payload)
			if err != nil {
				return nil, err
			}
			return append(out, b...), nil
		}
	case mime.MimeTypeH265:
		var h265 codecs.H265Depacketizer
		return func(out []byte, p *Packet) ([]byte, error) {
			b, err := h265.Unmarshal(p.Payload)
			if err != nil {
				return nil, err
			}
			return append(out, b...), nil
		}
	case mime.MimeTypeAV1:
		if encrypted {
			return AppendAV1Elements
		}
		var av1 codecs.AV1Depacketizer
		return func(out []byte, p *Packet) ([]byte, error) {
			b, err := av1.Unmarshal(p.Payload)
			if err != nil {
				return nil, err
			}
			return append(out, b...), nil
		}
	default:
		return func(out []byte, p *Packet) ([]byte, error) {
			return append(out, p.Payload...), nil
		}
	}
}

// AppendAV1Elements appends the OBU elements of an AV1 RTP payload as they
// were sent, without the size fields the AV1 depacketizer adds: the inverse
// of PacketizeAV1Opaque, for payloads that are not OBU sequences (E2EE).
func AppendAV1Elements(out []byte, p *Packet) ([]byte, error) {
	b := p.Payload
	if len(b) < 1 {
		return nil, errShortPayload
	}
	w := int(b[0]>>4) & 0x03
	b = b[1:]
	for i := 0; len(b) > 0; i++ {
		if w != 0 && i == w-1 {
			return append(out, b...), nil // the last element has no length
		}
		n, l := leb128(b)
		if l == 0 || uint64(len(b)-l) < n {
			return nil, errShortPayload
		}
		out = append(out, b[l:l+int(n)]...)
		b = b[l+int(n):]
	}
	return out, nil
}

func leb128(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 8; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// extractAV1TrailerFromSpans removes the packet trailer metadata OBU from an
// SVC picture's layers and rebuilds the frame data.
func extractAV1TrailerFromSpans(f *Frame, spans []LayerSpan) ([]byte, []LayerSpan) {
	found := false
	for i := range spans {
		if meta, stripped, ok := packettrailer.ExtractAV1(spans[i].Data); ok {
			if !found {
				f.Trailer, found = meta, true
			}
			spans[i].Data = stripped
		}
	}
	size := 0
	for _, sp := range spans {
		size += len(sp.Data)
	}
	data := make([]byte, 0, size)
	for i := range spans {
		start := len(data)
		data = append(data, spans[i].Data...)
		spans[i].Data = data[start:len(data):len(data)]
	}
	return data, spans
}
