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
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/utils/mono"
)

const (
	// DefaultProtectionPercent is the repair-to-media packet ratio when unspecified.
	DefaultProtectionPercent uint32 = 0
	MaxProtectionPercent     uint32 = 100
	// MediaPacketsPerGroup bounds both retained packet memory and encoding work.
	MediaPacketsPerGroup = 5
	maxEncoderGroupAge   = 200 * time.Millisecond
	// Reserve the FEC header and up to 16 bytes of outer RTP extensions (AST/TWCC).
	// Groups of at most 15 packets use only the first FlexFEC packet mask.
	maxEncoderMediaPacketSize = maxMediaPacketSize - pionflexfec.BaseFec03HeaderSize - 16
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
	packets [MediaPacketsPerGroup]rtp.Packet
	storage [MediaPacketsPerGroup][maxEncoderMediaPacketSize]byte
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
	repairCredit      uint32
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
	e.repairCredit = 0
}

// Encode returns independently owned repair packets for complete groups only.
// Gaps (including skipped padding), codec switches and stale groups start a new
// group, since Pion requires consecutive sequence numbers. RTP wrap is valid.
// Call after the media write, including any interceptor header modifications.
func (e *Encoder) Encode(header *rtp.Header, payload []byte) []rtp.Packet {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.protectionPercent == 0 {
		return nil
	}

	now := mono.UnixNano()
	size := header.MarshalSize() + len(payload)
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
			now-e.startedAt > int64(maxEncoderGroupAge) {
			e.count = 0
		}
	}
	if e.count == 0 {
		e.startedAt = now
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
	if e.count < MediaPacketsPerGroup {
		return nil
	}

	e.count = 0
	// Carry fractional packets between groups rather than rounding each group
	// up (which would turn e.g. 30% into 40%). Storage and latency stay bounded
	// at five media packets, independent of the configured percentage.
	e.repairCredit += e.protectionPercent * MediaPacketsPerGroup
	numRepair := e.repairCredit / 100
	e.repairCredit %= 100
	if numRepair == 0 {
		return nil
	}
	repair := e.encoder.EncodeFec(e.media.packets[:], numRepair)
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
// SSRC or sequence number. A new value discards partial groups and old credit.
// Values above 100 are capped; zero disables encoding before any packet copies.
func (e *Encoder) SetProtectionPercent(percent uint32) {
	percent = min(percent, MaxProtectionPercent)
	e.mu.Lock()
	if e.protectionPercent != percent {
		e.protectionPercent = percent
		e.count = 0
		e.repairCredit = 0
	}
	e.mu.Unlock()
}

// RecordSent reports successful repair writes, with RTP payload bytes like the
// upstream counters. Callbacks run outside the encoder lock.
func (e *Encoder) RecordSent(packets int, bytes int) {
	if e.onSent != nil && packets != 0 {
		e.onSent(packets, bytes)
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
