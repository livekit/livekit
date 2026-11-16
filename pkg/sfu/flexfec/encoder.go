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
	MaxProtectionPercent uint32 = 100
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
	mu             sync.Mutex
	encoder        *pionflexfec.FlexEncoder03
	media          *encoderMediaGroup
	count          int
	startedAt      int64
	sequenceNumber uint16
	ssrc           uint32
	closed         bool
	onSent         func(packets int, bytes int)

	protection Protection
	// adaptive is updated in every mode, like libwebrtc runs its adaptive
	// computation before applying a protection override, so that switching
	// to adaptive or minimum protection starts from the current loss history.
	adaptive *adaptiveState
	// effective Q8 repair-to-media packet ratios for delta and key frames
	deltaFactor uint8
	keyFactor   uint8
	// whether the frame with frameTimestamp contains key frame packets
	frameTimestamp uint32
	frameKey       bool

	overheadPercent   atomic.Uint32
	configuredPercent uint32
	measuredPercent   uint32
	overheadUpdatedAt int64
	sentMediaBytes    uint64
	sentRepairBytes   uint64
}

func NewEncoder(payloadType uint8, ssrc uint32, onSent func(packets int, bytes int)) *Encoder {
	return &Encoder{
		encoder:        pionflexfec.NewFlexEncoder03(payloadType, ssrc),
		sequenceNumber: uint16(rand.Uint32()),
		ssrc:           ssrc,
		onSent:         onSent,
		adaptive:       newAdaptiveState(mono.UnixNano()),
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
// keyFrame marks packets of key frames, which use the key frame protection
// factor; it is enough to mark one packet of each key frame.
// Call after the media write, including any interceptor header modifications.
func (e *Encoder) Encode(header *rtp.Header, payload []byte, keyFrame bool) []rtp.Packet {
	return e.encode(mono.UnixNano(), header, payload, keyFrame)
}

func (e *Encoder) encode(now int64, header *rtp.Header, payload []byte, keyFrame bool) []rtp.Packet {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}

	size := header.MarshalSize() + len(payload)
	if header.Timestamp != e.frameTimestamp {
		e.frameTimestamp = header.Timestamp
		e.frameKey = false
	}
	e.frameKey = e.frameKey || keyFrame
	e.adaptive.onMediaPacket(now, len(payload), header.Marker, e.frameKey)
	factor := e.deltaFactor
	if e.frameKey {
		factor = e.keyFactor
	}
	if factor != 0 {
		// Measure overhead only over media that is protected.
		e.sentMediaBytes += uint64(size)
	}
	if factor == 0 || header.Padding || len(payload) == 0 || size > maxEncoderMediaPacketSize {
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
	numRepair := max(uint32(1), (uint32(count)*uint32(factor)+128)>>8)
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

// SetProtection updates the requested protection without changing the repair
// SSRC or sequence number. Like libwebrtc, a group that is already
// accumulating is kept and protected with the new rate when it completes, and
// the adaptive state keeps its history. Overhead measurements restart.
// Disabling stops encoding before any packet copies.
func (e *Encoder) SetProtection(protection Protection) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if protection.Mode < ProtectionDisabled || protection.Mode > ProtectionMinimum {
		protection = Protection{}
	}
	if protection.Mode != ProtectionFixed && protection.Mode != ProtectionMinimum {
		protection.Factor = 0
	}
	if e.protection == protection {
		return
	}
	e.protection = protection
	e.measuredPercent = 0
	e.overheadUpdatedAt = 0
	e.sentMediaBytes = 0
	e.sentRepairBytes = 0
	e.setFactorsLocked(e.effectiveFactorsLocked())
}

// SetProtectionPercent sets a fixed repair-to-media packet percentage; values
// above 100 are capped and zero disables encoding.
func (e *Encoder) SetProtectionPercent(percent uint32) {
	if percent == 0 {
		e.SetProtection(Protection{})
		return
	}
	e.SetProtection(Protection{Mode: ProtectionFixed, Factor: ProtectionFactorFromPercent(percent)})
}

// NetworkStats are the receiver-side inputs of adaptive protection.
type NetworkStats struct {
	// FractionLost is the Q8 loss fraction from an RTCP reception report.
	FractionLost uint8
	RTT          time.Duration
	// Width and Height of the forwarded video, zero when unknown.
	Width  uint32
	Height uint32
}

// OnReceiverReport updates the adaptive state from a reception report of the
// protected media stream. It changes the effective rates only in adaptive and
// minimum modes, but is tracked in every mode.
func (e *Encoder) OnReceiverReport(stats NetworkStats) {
	e.onReceiverReport(mono.UnixNano(), stats)
}

func (e *Encoder) onReceiverReport(now int64, stats NetworkStats) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.adaptive.update(now, stats)
	e.setFactorsLocked(e.effectiveFactorsLocked())
}

// ProtectionFactors returns the effective Q8 repair-to-media packet ratios for
// delta and key frames.
func (e *Encoder) ProtectionFactors() (delta uint8, key uint8) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deltaFactor, e.keyFactor
}

// effectiveFactorsLocked applies the requested mode on top of the adaptive
// factors, like libwebrtc's ApplyProtectionOverride.
func (e *Encoder) effectiveFactorsLocked() (uint8, uint8) {
	switch e.protection.Mode {
	case ProtectionFixed:
		return e.protection.Factor, e.protection.Factor
	case ProtectionAdaptive:
		return e.adaptive.deltaFactor, e.adaptive.keyFactor
	case ProtectionMinimum:
		return max(e.adaptive.deltaFactor, e.protection.Factor), max(e.adaptive.keyFactor, e.protection.Factor)
	default:
		return 0, 0
	}
}

func (e *Encoder) setFactorsLocked(delta uint8, key uint8) {
	e.deltaFactor, e.keyFactor = delta, key
	if delta == 0 && key == 0 {
		// Nothing will be sent, so measurements would never refresh.
		e.count = 0
		e.measuredPercent = 0
		e.overheadUpdatedAt = 0
		e.sentMediaBytes = 0
		e.sentRepairBytes = 0
	}
	// Reserve for delta frames; key frames are rare and are not reserved for.
	e.configuredPercent = protectionFactorToPercent(delta)
	e.overheadPercent.Store(max(e.configuredPercent, e.measuredPercent))
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
	if !e.closed && e.protection.Mode != ProtectionDisabled {
		e.sentRepairBytes += uint64(rtpBytes)
		now := mono.UnixNano()
		if e.sentMediaBytes != 0 && (e.overheadUpdatedAt == 0 || now-e.overheadUpdatedAt >= int64(overheadUpdateInterval)) {
			e.measuredPercent = uint32((e.sentRepairBytes*100 + e.sentMediaBytes - 1) / e.sentMediaBytes)
			e.overheadPercent.Store(max(e.configuredPercent, e.measuredPercent))
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

// adaptiveState holds the inputs of libwebrtc's loss protection logic that
// the encoder measures itself: sent bit rate, frame rate and packets per frame.
type adaptiveState struct {
	loss lossFilter

	packetsPerFrame      expFilter
	packetsPerFrameKey   expFilter
	packetsPerFrameAt    int64
	packetsPerFrameKeyAt int64
	frameBytes           int
	windowStart          int64
	windowBytes          uint64
	windowFrames         uint64
	bitrateKbps          float64
	frameRate            float64
	deltaFactor          uint8
	keyFactor            uint8
}

// Use at least this window to measure the sent bit and frame rates.
const minRateWindow = 500 * time.Millisecond

func newAdaptiveState(now int64) *adaptiveState {
	return &adaptiveState{
		loss:                 newLossFilter(),
		packetsPerFrame:      newExpFilter(packetsPerFrameAlpha),
		packetsPerFrameKey:   newExpFilter(packetsPerFrameAlpha),
		packetsPerFrameAt:    now,
		packetsPerFrameKeyAt: now,
		windowStart:          now,
	}
}

func (a *adaptiveState) onMediaPacket(now int64, payloadSize int, marker bool, keyFrame bool) {
	a.windowBytes += uint64(payloadSize)
	a.frameBytes += payloadSize
	if !marker {
		return
	}
	a.windowFrames++
	// Like FecControllerDefault::UpdateWithEncodedData, estimate packets from
	// the frame size so that the key/delta ratio does not depend on packetization.
	packets := float64(a.frameBytes) / adaptiveMaxPayloadSize
	a.frameBytes = 0
	if keyFrame {
		a.packetsPerFrameKey.apply(float64(now-a.packetsPerFrameKeyAt)/1e6, packets)
		a.packetsPerFrameKeyAt = now
	} else {
		a.packetsPerFrame.apply(float64(now-a.packetsPerFrameAt)/1e6, packets)
		a.packetsPerFrameAt = now
	}
}

func (a *adaptiveState) update(now int64, stats NetworkStats) {
	if elapsed := now - a.windowStart; elapsed >= int64(minRateWindow) {
		elapsedMs := float64(elapsed) / 1e6
		// bits per millisecond is kbps
		a.bitrateKbps = float64(a.windowBytes) * 8 / elapsedMs
		a.frameRate = float64(a.windowFrames) * 1000 / elapsedMs
		a.windowStart = now
		a.windowBytes = 0
		a.windowFrames = 0
	}
	a.deltaFactor, a.keyFactor = adaptiveProtectionFactors(adaptiveParams{
		loss:               a.loss.filteredLoss(now/1e6, stats.FractionLost),
		rttMs:              stats.RTT.Milliseconds(),
		bitrateKbps:        a.bitrateKbps,
		frameRate:          a.frameRate,
		packetsPerFrame:    a.packetsPerFrame.filtered,
		packetsPerFrameKey: a.packetsPerFrameKey.filtered,
		width:              int(stats.Width),
		height:             int(stats.Height),
	})
}
