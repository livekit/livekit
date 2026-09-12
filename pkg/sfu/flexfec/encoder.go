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

package flexfec

import (
	"math/rand/v2"
	"sync"
	"time"

	pionflexfec "github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"
	"go.uber.org/atomic"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/utils/mono"
)

const (
	// DefaultProtectionPercent is the repair-to-media packet ratio when unspecified.
	DefaultProtectionPercent uint32 = 0
	MaxProtectionPercent     uint32 = 100
	// MaxMediaPacketsPerGroup bounds memory and work for large frames. Like
	// libwebrtc, use at most 48 media packets in a protection block.
	MaxMediaPacketsPerGroup = 48
	maxEncoderGroupAge      = 200 * time.Millisecond
	overheadUpdateInterval  = time.Second
	// Reserve all three FEC masks and up to 16 bytes of outer RTP extensions
	// (AST/TWCC). A 48-packet group can require the third mask.
	maxEncoderMediaPacketSize = maxMediaPacketSize - pionflexfec.BaseFec03HeaderSize - 12 - 16
)

// EncoderState preserves the repair sequence when a sender SSRC is reused.
// Partial media groups are never carried over to a new binding.
type EncoderState struct {
	SSRC               uint32
	NextSequenceNumber uint16
}

func (s EncoderState) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddUint32("SSRC", s.SSRC)
	e.AddUint16("NextSequenceNumber", s.NextSequenceNumber)
	return nil
}

type encoderMediaGroup struct {
	packets [MaxMediaPacketsPerGroup]rtp.Packet
	storage [MaxMediaPacketsPerGroup]*[maxEncoderMediaPacketSize]byte
}

// Encoder batches final, outgoing video packets for Pion's FlexFEC-03 encoder.
// It owns reusable wire storage: the caller may recycle headers, extensions and
// payloads as soon as Encode returns. It does not delay media or start workers.
// Methods are safe to call from concurrent pass-through pacer writes.
type Encoder struct {
	mu                sync.Mutex
	encoder           *pionflexfec.FlexEncoder03
	media             *encoderMediaGroup
	count             int
	startedAt         int64
	sequenceNumber    uint16
	ssrc              uint32
	closed            bool
	onSent            func(packets int, bytes int)
	protectionPercent uint32
	protectionFactor  uint32

	overheadPercent   atomic.Uint32
	overheadUpdatedAt int64
	sentMediaBytes    uint64
	sentRepairBytes   uint64
}

func NewEncoder(payloadType uint8, ssrc uint32, onSent func(packets int, bytes int)) *Encoder {
	return &Encoder{
		encoder:           pionflexfec.NewFlexEncoder03(payloadType, ssrc),
		sequenceNumber:    uint16(rand.Uint32()),
		ssrc:              ssrc,
		onSent:            onSent,
		protectionPercent: DefaultProtectionPercent,
	}
}

func (e *Encoder) GetState() EncoderState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return EncoderState{SSRC: e.ssrc, NextSequenceNumber: e.sequenceNumber}
}

// SeedState must be called before forwarding starts. Only reuse sequencing for
// the same repair SSRC; a new SSRC keeps its randomized initial sequence number.
func (e *Encoder) SeedState(state EncoderState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || state.SSRC == 0 || state.SSRC != e.ssrc {
		return
	}
	e.sequenceNumber = state.NextSequenceNumber
	e.count = 0
}

// Encode returns independently owned repairs at the end of a video frame (the
// RTP marker), splitting large frames at MaxMediaPacketsPerGroup. Timestamp
// changes discard an incomplete previous frame. Gaps, codec switches and stale
// groups also reset the block; Pion requires consecutive sequence numbers.
// Media is never delayed. No timer or worker is needed for sparse frames.
// Call after the media write, including any interceptor header modifications.
func (e *Encoder) Encode(header *rtp.Header, payload []byte) []rtp.Packet {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.protectionPercent == 0 {
		return nil
	}

	now := mono.UnixNano()
	size := header.MarshalSize() + len(payload)
	e.sentMediaBytes += uint64(size)
	if header.Padding || len(payload) == 0 || size > maxEncoderMediaPacketSize {
		e.count = 0
		return nil
	}
	if e.media == nil {
		// Like the upstream recovery buffer, allocate packet storage only once
		// it is needed. Negotiated tracks with the default "none" stay small.
		e.media = &encoderMediaGroup{}
	}
	if e.count != 0 {
		previous := &e.media.packets[e.count-1]
		if header.SequenceNumber != previous.SequenceNumber+1 ||
			header.SSRC != previous.SSRC || header.PayloadType != previous.PayloadType ||
			header.Timestamp != previous.Timestamp ||
			now-e.startedAt > int64(maxEncoderGroupAge) {
			e.count = 0
		}
	}
	if e.count == 0 {
		e.startedAt = now
	}

	if e.media.storage[e.count] == nil {
		// Retain only the packet buffers this stream has actually needed.
		e.media.storage[e.count] = new([maxEncoderMediaPacketSize]byte)
	}
	raw := e.media.storage[e.count][:size]
	n, err := header.MarshalTo(raw)
	if err != nil {
		e.count = 0
		return nil
	}
	copy(raw[n:], payload)
	if err = e.media.packets[e.count].Unmarshal(raw); err != nil {
		e.count = 0
		return nil
	}
	e.count++
	if !header.Marker && e.count < MaxMediaPacketsPerGroup {
		return nil
	}

	count := e.count
	e.count = 0
	// Match libwebrtc's ForwardErrorCorrection::NumFecPackets: round the Q8
	// protection factor, with at least one repair for every protected block.
	// In particular, a one-packet frame is protected immediately at every preset.
	numRepair := max(uint32(1), (uint32(count)*e.protectionFactor+128)>>8)
	repair := e.encoder.EncodeFec(e.media.packets[:count], numRepair)
	for i := range repair {
		// Pion's encoder uses a constant timestamp and a fixed initial sequence number.
		// Use this stream's media clock and a randomized, continuous repair sequence.
		repair[i].Timestamp = header.Timestamp
		repair[i].SequenceNumber = e.sequenceNumber
		e.sequenceNumber++
	}
	return repair
}

// SetProtectionPercent updates the packet ratio without changing the repair
// SSRC or sequence number. A new value discards partial groups and measurements.
// Values above 100 are capped; zero disables encoding before any packet copies.
func (e *Encoder) SetProtectionPercent(percent uint32) {
	percent = min(percent, MaxProtectionPercent)
	e.mu.Lock()
	if e.protectionPercent != percent {
		e.protectionPercent = percent
		// Publish-track presets use the same conversion to libwebrtc's Q8 rate.
		e.protectionFactor = percent * 255 / 100
		e.count = 0
		e.overheadPercent.Store(percent)
		e.overheadUpdatedAt = 0
		e.sentMediaBytes = 0
		e.sentRepairBytes = 0
	}
	e.mu.Unlock()
}

// OverheadPercent reserves at least the configured rate, using measured RTP
// bytes when frame rounding or repair headers increase the actual overhead.
// Reads by the allocator do not contend with encoding.
func (e *Encoder) OverheadPercent() uint32 {
	return e.overheadPercent.Load()
}

// RecordSent accounts successful repairs, including headers for the allocator
// and payload bytes for the upstream-compatible telemetry callback. Refresh the
// estimate on the first repair and at most once per second thereafter.
// Callbacks run outside the encoder lock.
func (e *Encoder) RecordSent(packets int, payloadBytes int, rtpBytes int) {
	if packets == 0 {
		return
	}
	e.mu.Lock()
	if !e.closed && e.protectionPercent != 0 {
		e.sentRepairBytes += uint64(rtpBytes)
		now := mono.UnixNano()
		if e.sentMediaBytes != 0 && (e.overheadUpdatedAt == 0 || now-e.overheadUpdatedAt >= int64(overheadUpdateInterval)) {
			percent := uint32((e.sentRepairBytes*100 + e.sentMediaBytes - 1) / e.sentMediaBytes)
			e.overheadPercent.Store(max(e.protectionPercent, percent))
			e.overheadUpdatedAt = now
			e.sentMediaBytes = 0
			e.sentRepairBytes = 0
		}
	}
	e.mu.Unlock()
	if e.onSent != nil {
		e.onSent(packets, payloadBytes)
	}
}

// Close prevents queued packets from generating repairs after unbind/close.
func (e *Encoder) Close() {
	e.mu.Lock()
	e.closed = true
	e.count = 0
	e.media = nil
	e.encoder = nil
	e.mu.Unlock()
}
