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

package buffer

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pion/rtp"

	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

// FlexFEC-03 (draft-ietf-payload-flexible-fec-scheme-03) decoder as sent by libwebrtc.
//
// A FlexFEC packet is an RTP packet on its own SSRC (negotiated via
// a=ssrc-group:FEC-FR <mediaSSRC> <fecSSRC>) whose payload is:
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|R|F| P|X|  CC   |M| PT recovery |         length recovery       |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                          TS recovery                          |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|   SSRCCount   |                    reserved                   |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                             SSRC_i                            |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|           SN base_i           |k|          Mask [0-14]        |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|k|                   Mask [15-45] (optional)                   |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|k|                                                             |
//	+-+                   Mask [46-108] (optional)                  |
//	|                                                               |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|              ... repair payload (XOR of protected) ...        |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// The first 10 bytes are the XOR of the protected packets' first 8 header
// bytes with their 16-bit (length - 12) in place of the sequence number,
// the repair payload is the XOR of the protected packets' bytes [12:],
// zero padded to the longest protected packet. A missing packet is recovered
// by XORing the FEC bit string and repair payload with all other protected
// packets, which therefore requires every other protected packet.

var (
	errFECPacketTruncated       = errors.New("flexfec packet truncated")
	errFECRetransmissionBit     = errors.New("flexfec retransmission bit set not supported")
	errFECInflexibleMask        = errors.New("flexfec inflexible generator matrix not supported")
	errFECMultipleSSRC          = errors.New("flexfec multiple ssrc protection not supported")
	errFECLastMaskKBitNotSet    = errors.New("flexfec k-bit of last optional mask not set")
	errFECEmptyMask             = errors.New("flexfec empty packet mask")
	errFECRepairPayloadTooSmall = errors.New("flexfec repair payload smaller than recovered length")
)

const (
	// FlexFEC-03 masks cover at most 109 packets from SN base
	flexFECMaxCoverage = 109

	defaultMaxPendingFEC = 64
)

// FECStreamStats are cumulative counters for one media stream's FlexFEC repair flow
type FECStreamStats struct {
	PacketsReceived     uint32
	PacketsInvalid      uint32
	RecoveryAttempts    uint32
	PacketsRecovered    uint32
	RecoveryFailed      uint32
	PacketsUnused       uint32
	PacketsDiscardedOld uint32
}

func (f FECStreamStats) String() string {
	return fmt.Sprintf(
		"received: %d, invalid: %d, recoveryAttempts: %d, recovered: %d, recoveryFailed: %d, unused: %d, discardedOld: %d",
		f.PacketsReceived, f.PacketsInvalid, f.RecoveryAttempts, f.PacketsRecovered, f.RecoveryFailed, f.PacketsUnused, f.PacketsDiscardedOld,
	)
}

type flexFECHeader struct {
	protectedSSRC uint32
	snBase        uint16
	// offsets from snBase of protected packets, ascending
	offsets []uint16
	// offset of the repair payload within the FEC RTP payload
	payloadOffset int
}

// parseFlexFEC03Header parses the FlexFEC-03 header out of a FEC RTP payload
func parseFlexFEC03Header(payload []byte) (flexFECHeader, error) {
	if len(payload) < 20 {
		return flexFECHeader{}, fmt.Errorf("%w: length %d", errFECPacketTruncated, len(payload))
	}

	if payload[0]&0x80 != 0 {
		return flexFECHeader{}, errFECRetransmissionBit
	}
	if payload[0]&0x40 != 0 {
		return flexFECHeader{}, errFECInflexibleMask
	}
	if ssrcCount := payload[8]; ssrcCount != 1 {
		return flexFECHeader{}, fmt.Errorf("%w: count %d", errFECMultipleSSRC, ssrcCount)
	}

	h := flexFECHeader{
		protectedSSRC: binary.BigEndian.Uint32(payload[12:]),
		snBase:        binary.BigEndian.Uint16(payload[16:]),
	}

	appendMask := func(mask uint64, bitCount uint16, baseOffset uint16) {
		for i := uint16(0); i < bitCount; i++ {
			if (mask>>(bitCount-1-i))&1 == 1 {
				h.offsets = append(h.offsets, baseOffset+i)
			}
		}
	}

	mask0 := binary.BigEndian.Uint16(payload[18:]) & 0x7fff
	appendMask(uint64(mask0), 15, 0)
	if payload[18]&0x80 != 0 {
		// k-bit 0 set, mask is 15 bits
		h.payloadOffset = 20
	} else {
		if len(payload) < 24 {
			return flexFECHeader{}, fmt.Errorf("%w: length %d", errFECPacketTruncated, len(payload))
		}
		appendMask(uint64(binary.BigEndian.Uint32(payload[20:])&0x7fffffff), 31, 15)
		if payload[20]&0x80 != 0 {
			// k-bit 1 set, masks are 15 + 31 bits
			h.payloadOffset = 24
		} else {
			// k-bit 2 must be set, masks are 15 + 31 + 63 bits
			if len(payload) < 32 {
				return flexFECHeader{}, fmt.Errorf("%w: length %d", errFECPacketTruncated, len(payload))
			}
			if payload[24]&0x80 == 0 {
				return flexFECHeader{}, errFECLastMaskKBitNotSet
			}
			appendMask(binary.BigEndian.Uint64(payload[24:])&0x7fffffffffffffff, 63, 46)
			h.payloadOffset = 32
		}
	}

	if len(h.offsets) == 0 {
		return flexFECHeader{}, errFECEmptyMask
	}
	return h, nil
}

type pendingFECPacket struct {
	fecSN       uint16
	header      flexFECHeader
	payload     []byte // copied FEC RTP payload
	arrivalTime int64
	// extended wire sequence numbers of protected packets, ascending,
	// resolved on first evaluation with an active sequence number reference
	protected []uint64
	// set once an evaluation saw no not-yet-arrived protected packets, after
	// which only arrivals of covered packets can change the outcome
	rangeSettled bool
}

type flexFECDecoderParams struct {
	Logger logger.Logger
	// SSRC of the protected media stream
	SSRC uint32
	// GetPacket fetches the stored raw packet for an extended wire sequence number
	GetPacket func(buf []byte, wireESN uint64) (int, error)
	// ExtHighestSN returns the extended highest wire sequence number received, false if none yet
	ExtHighestSN func() (uint64, bool)
	// WindowSize returns the number of packets retrievable via GetPacket
	WindowSize func() int
	// MaxPendingFEC bounds the number of buffered FEC packets, 0 for default
	MaxPendingFEC int
}

// flexFECDecoder recovers lost media packets from a FlexFEC-03 repair flow.
// Received media packets are not duplicated here, presence and XOR inputs
// are sourced from the primary buffer's packet bucket via GetPacket.
// All methods must be called holding the primary buffer's lock.
type flexFECDecoder struct {
	params flexFECDecoderParams

	pending []*pendingFECPacket
	stats   FECStreamStats

	pktBuf     []byte // scratch for GetPacket
	payloadAcc []byte // scratch for repair payload XOR accumulation

	// packets recovered within the current call, recovery of the same
	// packet by an overlapping FEC is deferred until it is injected
	justRecovered map[uint64]struct{}
}

func newFlexFECDecoder(params flexFECDecoderParams) *flexFECDecoder {
	if params.MaxPendingFEC == 0 {
		params.MaxPendingFEC = defaultMaxPendingFEC
	}
	return &flexFECDecoder{
		params:        params,
		pktBuf:        make([]byte, bucket.RTPMaxPktSize),
		payloadAcc:    make([]byte, bucket.RTPMaxPktSize),
		justRecovered: make(map[uint64]struct{}, 4),
	}
}

func (d *flexFECDecoder) HasPending() bool {
	return len(d.pending) > 0
}

func (d *flexFECDecoder) Stats() FECStreamStats {
	return d.stats
}

// AddFEC processes one received FlexFEC packet and returns raw recovered media
// packets, if any. The payload is copied and may be reused by the caller.
func (d *flexFECDecoder) AddFEC(fecSN uint16, payload []byte, arrivalTime int64) [][]byte {
	d.stats.PacketsReceived++
	prometheus.IncrementFEC(prometheus.FECStateReceived, 1)

	for _, p := range d.pending {
		if p.fecSN == fecSN {
			// duplicate of a buffered FEC packet
			return nil
		}
	}

	header, err := parseFlexFEC03Header(payload)
	if err == nil && header.protectedSSRC != d.params.SSRC {
		err = fmt.Errorf("flexfec protecting unexpected ssrc, expected %d, got %d", d.params.SSRC, header.protectedSSRC)
	}
	if err != nil {
		d.stats.PacketsInvalid++
		prometheus.IncrementFEC(prometheus.FECStateInvalid, 1)
		d.params.Logger.Debugw("dropping invalid flexfec packet", "error", err, "fecSN", fecSN)
		return nil
	}

	defer clear(d.justRecovered)
	recoveredPackets := d.evictStale()

	if len(d.pending) >= d.params.MaxPendingFEC {
		d.pending = d.pending[1:]
		d.stats.PacketsDiscardedOld++
		prometheus.IncrementFEC(prometheus.FECStateDiscardedOld, 1)
	}

	p := &pendingFECPacket{
		fecSN:       fecSN,
		header:      header,
		payload:     append([]byte(nil), payload...),
		arrivalTime: arrivalTime,
	}
	d.pending = append(d.pending, p)

	if recovered := d.evaluate(p); recovered != nil {
		recoveredPackets = append(recoveredPackets, recovered)
	}
	return recoveredPackets
}

// OnMediaPacket re-evaluates buffered FEC packets covering the given wire
// sequence number and returns raw recovered media packets, if any.
// Callers should check HasPending first to keep the common path cheap.
func (d *flexFECDecoder) OnMediaPacket(wireSN uint16) [][]byte {
	if len(d.pending) == 0 {
		return nil
	}

	ref, ok := d.params.ExtHighestSN()
	if !ok {
		return nil
	}
	esn := unwrapNearESN(wireSN, ref)

	var recoveredPackets [][]byte
	defer clear(d.justRecovered)
	for _, p := range append([]*pendingFECPacket(nil), d.pending...) {
		if !p.affectedBy(esn) {
			continue
		}
		if recovered := d.evaluate(p); recovered != nil {
			recoveredPackets = append(recoveredPackets, recovered)
		}
	}
	return recoveredPackets
}

// affectedBy reports whether the arrival of the given packet can change the
// outcome of this FEC packet, either because it is protected by it or because
// it moves a previously not-yet-arrived part of the protected range into the
// loss-detectable past
func (p *pendingFECPacket) affectedBy(esn uint64) bool {
	if p.protected == nil {
		// not resolved yet, conservatively assume affected
		return true
	}
	if !p.rangeSettled && esn > p.protected[len(p.protected)-1] {
		return true
	}
	for _, protectedESN := range p.protected {
		if protectedESN == esn {
			return true
		}
	}
	return false
}

// evictStale drops buffered FEC packets whose protected range has fallen out
// of the packet window, giving each a final evaluation first. Returns any
// packets recovered by those final evaluations.
func (d *flexFECDecoder) evictStale() [][]byte {
	ref, ok := d.params.ExtHighestSN()
	if !ok {
		return nil
	}

	var recoveredPackets [][]byte
	window := uint64(d.params.WindowSize())
	for _, p := range append([]*pendingFECPacket(nil), d.pending...) {
		if p.protected == nil {
			continue
		}
		if ref > p.protected[0]+window {
			// oldest protected packet is leaving the window, final attempt;
			// evaluate evicts the packet on any conclusive outcome
			if recovered := d.evaluate(p); recovered != nil {
				recoveredPackets = append(recoveredPackets, recovered)
			}
			d.remove(p, prometheus.FECStateRecoveryFailed)
		}
	}
	return recoveredPackets
}

// remove drops a pending FEC packet, counting the disposition if it is still buffered
func (d *flexFECDecoder) remove(p *pendingFECPacket, state prometheus.FECState) {
	for i, pp := range d.pending {
		if pp != p {
			continue
		}
		d.pending = append(d.pending[:i], d.pending[i+1:]...)
		switch state {
		case prometheus.FECStateUnused:
			d.stats.PacketsUnused++
			prometheus.IncrementFEC(state, 1)
		case prometheus.FECStateRecoveryFailed:
			d.stats.RecoveryFailed++
			prometheus.IncrementFEC(state, 1)
		case prometheus.FECStateDiscardedOld:
			d.stats.PacketsDiscardedOld++
			prometheus.IncrementFEC(state, 1)
		}
		return
	}
}

// evaluate classifies the protected packets of a buffered FEC packet and runs
// XOR recovery when exactly one is missing, returning the recovered raw packet.
// Conclusive outcomes (recovered, unused, unrecoverable) evict the FEC packet.
func (d *flexFECDecoder) evaluate(p *pendingFECPacket) []byte {
	ref, ok := d.params.ExtHighestSN()
	if !ok {
		// no media received yet, keep pending
		return nil
	}

	if p.protected == nil {
		p.protected = make([]uint64, 0, len(p.header.offsets))
		for _, offset := range p.header.offsets {
			p.protected = append(p.protected, unwrapNearESN(p.header.snBase+offset, ref))
		}
	}

	var (
		missing        int
		missingESN     uint64
		hdrAcc         [12]byte
		repair         = p.payload[p.header.payloadOffset:]
		payloadAccSize = len(repair)
	)
	if payloadAccSize > len(d.payloadAcc) {
		payloadAccSize = len(d.payloadAcc)
	}
	copy(hdrAcc[:10], p.payload[:10])
	copy(d.payloadAcc[:payloadAccSize], repair[:payloadAccSize])

	for _, esn := range p.protected {
		if _, isJustRecovered := d.justRecovered[esn]; isJustRecovered {
			// recovered by an overlapping FEC in this call but not yet in the
			// bucket, defer to the re-evaluation triggered by its injection
			return nil
		}

		n, err := d.params.GetPacket(d.pktBuf, esn)
		switch {
		case err == nil:
			if n < 12 {
				// not a parseable RTP packet, cannot use as XOR input
				d.remove(p, prometheus.FECStateRecoveryFailed)
				return nil
			}
			raw := d.pktBuf[:n]
			hdrAcc[0] ^= raw[0]
			hdrAcc[1] ^= raw[1]
			lengthRecovery := uint16(n - 12)
			hdrAcc[2] ^= byte(lengthRecovery >> 8)
			hdrAcc[3] ^= byte(lengthRecovery)
			for i := 4; i < 8; i++ {
				hdrAcc[i] ^= raw[i]
			}
			for i := 12; i < n && i-12 < payloadAccSize; i++ {
				d.payloadAcc[i-12] ^= raw[i]
			}

		case errors.Is(err, bucket.ErrPacketSizeInvalid):
			// in-window hole, the packet was lost
			missing++
			missingESN = esn
			if missing > 1 {
				// not recoverable yet, keep pending
				return nil
			}

		case errors.Is(err, bucket.ErrPacketTooNew):
			// not arrived yet, keep pending and re-evaluate as the stream advances
			return nil

		default:
			// packet aged out of the window or was excluded (e.g. a received
			// padding-only packet that is not stored), never recoverable
			d.remove(p, prometheus.FECStateRecoveryFailed)
			return nil
		}
	}
	// the classification completed without not-yet-arrived packets
	p.rangeSettled = true

	if missing == 0 {
		d.remove(p, prometheus.FECStateUnused)
		return nil
	}

	recovered, err := d.finishRecovery(p, hdrAcc, d.payloadAcc[:payloadAccSize], missingESN)
	d.stats.RecoveryAttempts++
	prometheus.IncrementFEC(prometheus.FECStateRecoveryAttempt, 1)
	if err != nil {
		d.params.Logger.Debugw("flexfec recovery failed", "error", err, "fecSN", p.fecSN, "missingESN", missingESN)
		d.remove(p, prometheus.FECStateRecoveryFailed)
		return nil
	}

	d.stats.PacketsRecovered++
	prometheus.IncrementFEC(prometheus.FECStateRecovered, 1)
	d.justRecovered[missingESN] = struct{}{}
	// the FEC packet did its job, no eviction disposition to count
	d.remove(p, "")
	return recovered
}

// finishRecovery turns the XOR accumulators into the recovered raw packet
func (d *flexFECDecoder) finishRecovery(p *pendingFECPacket, hdrAcc [12]byte, payloadAcc []byte, missingESN uint64) ([]byte, error) {
	// force RTP version 2
	hdrAcc[0] = (hdrAcc[0] | 0x80) & 0xbf

	recoveredLen := int(binary.BigEndian.Uint16(hdrAcc[2:4]))
	if recoveredLen > len(payloadAcc) {
		return nil, fmt.Errorf("%w: recovered %d, repair %d", errFECRepairPayloadTooSmall, recoveredLen, len(payloadAcc))
	}

	binary.BigEndian.PutUint16(hdrAcc[2:4], uint16(missingESN))
	binary.BigEndian.PutUint32(hdrAcc[8:12], d.params.SSRC)

	recovered := make([]byte, 12+recoveredLen)
	copy(recovered, hdrAcc[:])
	copy(recovered[12:], payloadAcc[:recoveredLen])

	var pkt rtp.Packet
	if err := pkt.Unmarshal(recovered); err != nil {
		return nil, fmt.Errorf("recovered packet does not unmarshal: %w", err)
	}
	return recovered, nil
}

// unwrapNearESN expands a 16-bit wire sequence number to the extended sequence
// number closest to the reference
func unwrapNearESN(sn uint16, ref uint64) uint64 {
	candidate := (ref &^ uint64(0xffff)) | uint64(sn)
	if candidate > ref {
		if candidate-ref > 0x8000 && candidate >= (1<<16) {
			candidate -= 1 << 16
		}
	} else if ref-candidate > 0x8000 {
		candidate += 1 << 16
	}
	return candidate
}
