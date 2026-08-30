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

// Package flexfec implements a FlexFEC-03 decoder used to recover RTP packets
// lost on the publisher leg before they are forwarded downstream.
// https://datatracker.ietf.org/doc/html/draft-ietf-payload-flexible-fec-scheme-03
//
// The recovery logic is ported from pion/interceptor pkg/flexfec
// (https://github.com/pion/interceptor, MIT License, Copyright The Pion
// community), which is itself modeled on libwebrtc's ForwardErrorCorrection
// receiver. Deviations from the pion implementation:
//   - FEC packets are deep-copied only when retained (callers reuse packet
//     memory), while protected media is read from the owning packet store
//   - packet masks are expanded on demand instead of retaining per-packet
//     protection entries
//   - recovery XORs the stored RTP wire representation directly
//   - failed recoveries are not emitted as empty packets
//   - usage counters for metrics
package flexfec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"github.com/livekit/protocol/logger"
)

var (
	errPacketTruncated                = errors.New("packet truncated")
	errRetransmissionBitSet           = errors.New("packet with retransmission bit set not supported")
	errInflexibleGeneratorMatrix      = errors.New("packet with inflexible generator matrix not supported")
	errMultipleSSRCProtection         = errors.New("multiple ssrc protection not supported")
	errLastOptionalMaskKBitSetToFalse = errors.New("k-bit of last optional mask is set to false")
	errEmptyMask                      = errors.New("empty fec packet mask")
	errUnknownProtectedSSRC           = errors.New("fec is protecting unknown ssrc")
	errMediaPacketNotFound            = errors.New("protected media packet not found")
	errInvalidRecoveredPacketSize     = errors.New("invalid recovered packet size")
)

const (
	// number of media arrivals before sequence gaps trigger a state reset
	mediaPacketsBeforeGapCheck = 100
	// maximum number of FEC packets retained
	maxFECPackets = 100
	// maximum number of sequence numbers represented by the three packet masks
	maxProtectedPackets = fecMask0Bits + fecMask1Bits + fecMask2Bits
	// matches the maximum packet size of the primary RTP packet bucket
	maxMediaPacketSize = 1500
)

// FlexFEC-03 header bit fields.
// https://datatracker.ietf.org/doc/html/draft-ietf-payload-flexible-fec-scheme-03#section-6.1
const (
	fecRetransmissionBit = 0x80 // R bit, first FEC header byte
	fecInflexibleBit     = 0x40 // F bit, first FEC header byte
	fecMaskKBit          = 0x80 // K bit, terminates the run of packet-mask chunks

	// Data-bit width of each packet-mask chunk (the chunk minus its K bit).
	fecMask0Bits = 15
	fecMask1Bits = 31
	fecMask2Bits = 63

	// Value masks that clear the K bit from each packet-mask chunk.
	fecMask0Value = 0x7FFF
	fecMask1Value = 0x7FFFFFFF
	fecMask2Value = 0x7FFFFFFFFFFFFFFF
)

// DecoderStats accumulates FEC usage counters. Snapshot via Decoder.Stats.
type DecoderStats struct {
	// FEC packets fed to the decoder
	FECPacketsReceived uint64
	// FEC bytes fed to the decoder (RTP payload sizes)
	FECBytesReceived uint64
	// FEC packets that could not be used: parse failures, foreign protected
	// SSRC, empty masks and duplicates
	FECPacketsDiscarded uint64
	// media packets reconstructed from FEC
	PacketsRecovered uint64
}

// MediaPacketLookup copies a protected media packet into dst. The caller
// serializes access with writes to the underlying packet store.
type MediaPacketLookup func(sequenceNumber uint16, dst []byte) (int, error)

// Decoder recovers lost media packets of a single protected SSRC from a
// FlexFEC-03 repair stream. It is not safe for concurrent use; the owning
// buffer serializes access.
type Decoder struct {
	logger             logger.Logger
	fecSSRC            uint32
	protectedSSRC      uint32
	mediaPacketLookup  MediaPacketLookup
	mediaPacketBuf     [maxMediaPacketSize]byte
	newestMediaSeq     uint16
	mediaPacketsSeen   int
	hasNewestMediaSeq  bool
	receivedFECPackets []fecPacketState
	stats              DecoderStats
}

func NewDecoder(
	fecSSRC uint32,
	protectedSSRC uint32,
	mediaPacketLookup MediaPacketLookup,
	logger logger.Logger,
) *Decoder {
	return &Decoder{
		logger:            logger,
		fecSSRC:           fecSSRC,
		protectedSSRC:     protectedSSRC,
		mediaPacketLookup: mediaPacketLookup,
	}
}

func (d *Decoder) Stats() DecoderStats {
	return d.stats
}

// DecodeFEC ingests a packet of either the FEC stream (fecSSRC) or the
// protected media stream (protectedSSRC) and returns any media packets that
// became recoverable. Ownership of returned packets transfers to the caller.
func (d *Decoder) DecodeFEC(receivedPacket *rtp.Packet) []*rtp.Packet {
	switch receivedPacket.SSRC {
	case d.fecSSRC:
		d.stats.FECPacketsReceived++
		d.stats.FECBytesReceived += uint64(len(receivedPacket.Payload))
		d.discardOldFECPackets(receivedPacket.SequenceNumber)
		d.insertFECPacket(receivedPacket)
	case d.protectedSSRC:
		d.observeMediaPacket(receivedPacket.SequenceNumber)
	default:
		return nil
	}

	recovered := d.attemptRecovery()
	d.stats.PacketsRecovered += uint64(len(recovered))
	return recovered
}

func (d *Decoder) observeMediaPacket(sequenceNumber uint16) {
	if d.hasNewestMediaSeq && d.mediaPacketsSeen >= mediaPacketsBeforeGapCheck &&
		seqDiff(sequenceNumber, d.newestMediaSeq) > uint16(mediaPacketsBeforeGapCheck) {
		d.logger.Infow("flexfec: big gap in media sequence numbers - resetting buffers")
		d.receivedFECPackets = nil
		d.mediaPacketsSeen = 0
		d.newestMediaSeq = sequenceNumber
	}

	if !d.hasNewestMediaSeq || isNewerSeq(d.newestMediaSeq, sequenceNumber) {
		d.newestMediaSeq = sequenceNumber
		d.hasNewestMediaSeq = true
	}
	if d.mediaPacketsSeen < mediaPacketsBeforeGapCheck {
		d.mediaPacketsSeen++
	}
}

func (d *Decoder) discardOldFECPackets(sequenceNumber uint16) {
	// Keep the retained sequence-number span well below half of the sequence
	// space. This keeps ordering unambiguous across wrap-around and reduces the
	// possibility of decoding against stale state.
	if len(d.receivedFECPackets) > 0 {
		toRemove := 0
		for _, fecPkt := range d.receivedFECPackets {
			if seqDiff(sequenceNumber, fecPkt.packet.SequenceNumber) > 0x3fff {
				toRemove++
			} else {
				// no need to keep iterating, since receivedFECPackets is sorted
				break
			}
		}
		if toRemove > 0 {
			clear(d.receivedFECPackets[:toRemove])
			d.receivedFECPackets = d.receivedFECPackets[toRemove:]
		}
	}
}

func (d *Decoder) insertFECPacket(fecPkt *rtp.Packet) {
	for i := range d.receivedFECPackets {
		if d.receivedFECPackets[i].packet.SequenceNumber == fecPkt.SequenceNumber {
			d.stats.FECPacketsDiscarded++
			return
		}
	}

	fec, err := parseFlexFEC03Header(fecPkt.Payload)
	if err != nil {
		d.stats.FECPacketsDiscarded++
		d.logger.Debugw("flexfec: failed to parse header", "error", err)
		return
	}

	if fec.protectedSSRC != d.protectedSSRC {
		d.stats.FECPacketsDiscarded++
		d.logger.Debugw(
			"flexfec: discarding packet protecting foreign ssrc",
			"error", errUnknownProtectedSSRC,
			"expectedSSRC", d.protectedSSRC,
			"protectedSSRC", fec.protectedSSRC,
		)
		return
	}

	if fec.mask0 == 0 && fec.mask1 == 0 && fec.mask2 == 0 {
		d.stats.FECPacketsDiscarded++
		d.logger.Debugw("flexfec: discarding packet", "error", errEmptyMask)
		return
	}

	if d.countMissingPackets(fec, nil) == 0 {
		return
	}

	// The caller may reuse packet memory after DecodeFEC returns. Take
	// ownership only now that this FEC state needs to be retained.
	ownedFECPkt := fecPkt.Clone()
	ownedFEC := fec
	ownedFEC.payload = ownedFECPkt.Payload[len(fecPkt.Payload)-len(fec.payload):]

	state := fecPacketState{packet: ownedFECPkt, flexFEC: ownedFEC}
	d.receivedFECPackets = append(d.receivedFECPackets, state)
	if len(d.receivedFECPackets) > 1 && !isNewerSeq(
		d.receivedFECPackets[len(d.receivedFECPackets)-2].packet.SequenceNumber,
		state.packet.SequenceNumber,
	) {
		insertAt := sort.Search(len(d.receivedFECPackets)-1, func(i int) bool {
			return isNewerSeq(state.packet.SequenceNumber, d.receivedFECPackets[i].packet.SequenceNumber)
		})
		copy(d.receivedFECPackets[insertAt+1:], d.receivedFECPackets[insertAt:len(d.receivedFECPackets)-1])
		d.receivedFECPackets[insertAt] = state
	}

	if len(d.receivedFECPackets) > maxFECPackets {
		d.removeFECPacketAt(0)
	}
}

func (d *Decoder) attemptRecovery() []*rtp.Packet {
	var recoveredPackets []*rtp.Packet
	for {
		packetsRecovered := 0
		for i := 0; i < len(d.receivedFECPackets); {
			fecPkt := &d.receivedFECPackets[i]
			packetsMissing := d.countMissingPackets(fecPkt.flexFEC, recoveredPackets)
			if packetsMissing == 0 {
				d.removeFECPacketAt(i)
				continue
			}

			if packetsMissing != 1 {
				i++
				continue
			}

			recovered, err := d.recoverPacket(fecPkt, recoveredPackets)
			if err != nil {
				d.logger.Debugw("flexfec: failed to recover packet", "error", err)
				i++
				continue
			}

			d.removeFECPacketAt(i)
			recoveredPackets = append(recoveredPackets, recovered)
			packetsRecovered++
		}

		if packetsRecovered == 0 {
			break
		}
	}

	return recoveredPackets
}

func (d *Decoder) countMissingPackets(fec flexFEC, recoveredPackets []*rtp.Packet) int {
	var protectedSeqBuf [maxProtectedPackets]uint16
	protectedSeqs := fec.protectedSequences(protectedSeqBuf[:0])
	missing := 0
	for _, sequenceNumber := range protectedSeqs {
		if _, err := d.getMediaPacket(sequenceNumber, recoveredPackets, d.mediaPacketBuf[:]); err != nil {
			missing++
			if missing > 1 {
				break
			}
		}
	}

	return missing
}

func (d *Decoder) getMediaPacket(
	sequenceNumber uint16,
	recoveredPackets []*rtp.Packet,
	dst []byte,
) (int, error) {
	for _, recoveredPacket := range recoveredPackets {
		if recoveredPacket.SequenceNumber == sequenceNumber {
			return recoveredPacket.MarshalTo(dst)
		}
	}
	if d.mediaPacketLookup == nil {
		return 0, errMediaPacketNotFound
	}

	return d.mediaPacketLookup(sequenceNumber, dst)
}

func (d *Decoder) removeFECPacketAt(index int) {
	last := len(d.receivedFECPackets) - 1
	copy(d.receivedFECPackets[index:], d.receivedFECPackets[index+1:])
	d.receivedFECPackets[last] = fecPacketState{}
	d.receivedFECPackets = d.receivedFECPackets[:last]
}

func (d *Decoder) recoverPacket(fec *fecPacketState, recoveredPackets []*rtp.Packet) (*rtp.Packet, error) {
	// https://datatracker.ietf.org/doc/html/draft-ietf-payload-flexible-fec-scheme-03#section-6.3.2

	// 2. For the repair packet in T, extract the FEC bit string as the
	//    first 80 bits of the FEC header.
	var headerRecovery [12]byte
	copy(headerRecovery[:], fec.packet.Payload[:10])
	var protectedSeqBuf [maxProtectedPackets]uint16
	protectedSeqs := fec.flexFEC.protectedSequences(protectedSeqBuf[:0])

	missing := 0
	var sequenceNumber uint16
	for _, protectedSeq := range protectedSeqs {
		n, err := d.getMediaPacket(protectedSeq, recoveredPackets, d.mediaPacketBuf[:])
		if err != nil {
			missing++
			sequenceNumber = protectedSeq
			continue
		}
		if n < 12 {
			return nil, fmt.Errorf("%w: protected packet length %d", errInvalidRecoveredPacketSize, n)
		}

		// 1. For each source packet received in T, XOR the first 64 header
		// bits with the sequence-number field replaced by the packet length
		// after the fixed 12-byte RTP header.
		packet := d.mediaPacketBuf[:n]
		headerRecovery[0] ^= packet[0]
		headerRecovery[1] ^= packet[1]
		packetLength := uint16(n - 12) // #nosec G115 -- RTP packet size is bounded above
		headerRecovery[2] ^= byte(packetLength >> 8)
		headerRecovery[3] ^= byte(packetLength)
		for i := 4; i < 8; i++ {
			headerRecovery[i] ^= packet[i]
		}
	}
	if missing != 1 {
		return nil, fmt.Errorf("cannot recover with %d missing packets", missing)
	}

	// set version to 2
	headerRecovery[0] |= 0x80
	headerRecovery[0] &= 0xbf
	payloadLength := binary.BigEndian.Uint16(headerRecovery[2:4])
	if int(payloadLength)+12 > maxMediaPacketSize {
		return nil, fmt.Errorf("%w: recovered packet length %d", errInvalidRecoveredPacketSize, int(payloadLength)+12)
	}
	binary.BigEndian.PutUint16(headerRecovery[2:4], sequenceNumber)
	binary.BigEndian.PutUint32(headerRecovery[8:12], d.protectedSSRC)

	recoveredRaw := make([]byte, 12+int(payloadLength))
	copy(recoveredRaw[:12], headerRecovery[:])
	copy(recoveredRaw[12:], fec.flexFEC.payload)
	for _, protectedSeq := range protectedSeqs {
		n, err := d.getMediaPacket(protectedSeq, recoveredPackets, d.mediaPacketBuf[:])
		if err != nil {
			continue
		}
		if n < 12 {
			return nil, fmt.Errorf("%w: protected packet length %d", errInvalidRecoveredPacketSize, n)
		}
		for i := 0; i < min(int(payloadLength), n-12); i++ {
			recoveredRaw[12+i] ^= d.mediaPacketBuf[12+i]
		}
	}

	packet := &rtp.Packet{}
	if err := packet.Unmarshal(recoveredRaw); err != nil {
		return nil, fmt.Errorf("unmarshal recovered: %w", err)
	}

	return packet, nil
}

func appendMaskSequences(dst []uint16, mask uint64, bitCount uint16, seqNumBase uint16) []uint16 {
	for i := uint16(0); i < bitCount; i++ {
		if (mask>>(bitCount-1-i))&1 == 1 {
			dst = append(dst, seqNumBase+i)
		}
	}

	return dst
}

type fecPacketState struct {
	packet  *rtp.Packet
	flexFEC flexFEC
}

type flexFEC struct {
	protectedSSRC uint32
	seqNumBase    uint16
	mask0         uint16
	mask1         uint32
	mask2         uint64
	payload       []byte
}

func (f flexFEC) protectedSequences(dst []uint16) []uint16 {
	dst = appendMaskSequences(dst, uint64(f.mask0), fecMask0Bits, f.seqNumBase)
	if f.mask1 != 0 {
		dst = appendMaskSequences(dst, uint64(f.mask1), fecMask1Bits, f.seqNumBase+fecMask0Bits)
	}
	if f.mask2 != 0 {
		dst = appendMaskSequences(dst, f.mask2, fecMask2Bits, f.seqNumBase+fecMask0Bits+fecMask1Bits)
	}

	return dst
}

func parseFlexFEC03Header(data []byte) (flexFEC, error) {
	if len(data) < 20 {
		return flexFEC{}, fmt.Errorf("%w: length %d", errPacketTruncated, len(data))
	}

	rBit := (data[0] & fecRetransmissionBit) != 0
	if rBit {
		return flexFEC{}, errRetransmissionBitSet
	}

	fBit := (data[0] & fecInflexibleBit) != 0
	if fBit {
		return flexFEC{}, errInflexibleGeneratorMatrix
	}

	ssrcCount := data[8]
	if ssrcCount != 1 {
		return flexFEC{}, fmt.Errorf("%w: count %d", errMultipleSSRCProtection, ssrcCount)
	}

	protectedSSRC := binary.BigEndian.Uint32(data[12:])
	seqNumBase := binary.BigEndian.Uint16(data[16:])
	rawPacketMask := data[18:]
	var payload []byte

	kBit0 := (rawPacketMask[0] & fecMaskKBit) != 0
	maskPart0 := binary.BigEndian.Uint16(rawPacketMask[0:2]) & fecMask0Value
	var maskPart1 uint32
	var maskPart2 uint64

	if kBit0 {
		payload = rawPacketMask[2:]
	} else {
		if len(data) < 24 {
			return flexFEC{}, fmt.Errorf("%w: length %d", errPacketTruncated, len(data))
		}

		kBit1 := (rawPacketMask[2] & fecMaskKBit) != 0
		maskPart1 = binary.BigEndian.Uint32(rawPacketMask[2:]) & fecMask1Value

		if kBit1 {
			payload = rawPacketMask[6:]
		} else {
			if len(data) < 32 {
				return flexFEC{}, fmt.Errorf("%w: length %d", errPacketTruncated, len(data))
			}

			kBit2 := (rawPacketMask[6] & fecMaskKBit) != 0
			maskPart2 = binary.BigEndian.Uint64(rawPacketMask[6:]) & fecMask2Value

			if kBit2 {
				payload = rawPacketMask[14:]
			} else {
				return flexFEC{}, errLastOptionalMaskKBitSetToFalse
			}
		}
	}

	return flexFEC{
		protectedSSRC: protectedSSRC,
		seqNumBase:    seqNumBase,
		mask0:         maskPart0,
		mask1:         maskPart1,
		mask2:         maskPart2,
		payload:       payload,
	}, nil
}

func seqDiff(a, b uint16) uint16 {
	return min(a-b, b-a)
}

func isNewerSeq(prevValue, value uint16) bool {
	// half-way mark
	breakpoint := uint16(0x8000)
	if value-prevValue == breakpoint {
		return value > prevValue
	}

	return value != prevValue && (value-prevValue) < breakpoint
}
