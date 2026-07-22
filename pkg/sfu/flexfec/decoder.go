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
//   - packets are deep-copied on insertion (callers reuse packet memory)
//   - the media window holds stable heap pointers; the pion version keeps
//     values and re-sorts them in place, which invalidates the references
//     held by FEC packet state on out-of-order arrival
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
)

const (
	// media window size that triggers the sequence gap reset check
	maxMediaPackets = 100
	// maximum number of FEC packets retained
	maxFECPackets = 100
	// seen/recovered media packets retained for XOR recovery
	recoveredPacketsLimit = 192
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

// Decoder recovers lost media packets of a single protected SSRC from a
// FlexFEC-03 repair stream. It is not safe for concurrent use; the owning
// buffer serializes access.
type Decoder struct {
	logger             logger.Logger
	fecSSRC            uint32
	protectedSSRC      uint32
	recoveredPackets   []*rtp.Packet
	receivedFECPackets []fecPacketState
	stats              DecoderStats
}

func NewDecoder(fecSSRC uint32, protectedSSRC uint32, logger logger.Logger) *Decoder {
	return &Decoder{
		logger:        logger,
		fecSSRC:       fecSSRC,
		protectedSSRC: protectedSSRC,
	}
}

func (d *Decoder) Stats() DecoderStats {
	return d.stats
}

// DecodeFec ingests a packet of either the FEC stream (fecSSRC) or the
// protected media stream (protectedSSRC) and returns any media packets that
// became recoverable. Returned packets are owned by the decoder's internal
// window; callers must not mutate them.
func (d *Decoder) DecodeFec(receivedPacket *rtp.Packet) []*rtp.Packet {
	if receivedPacket.SSRC == d.fecSSRC {
		d.stats.FECPacketsReceived++
		d.stats.FECBytesReceived += uint64(len(receivedPacket.Payload))
	}

	// the caller reuses packet memory, keep an owned copy
	pkt := receivedPacket.Clone()

	if len(d.recoveredPackets) >= maxMediaPackets {
		backRecoveredPacket := d.recoveredPackets[len(d.recoveredPackets)-1]
		if backRecoveredPacket.SSRC == pkt.SSRC {
			if seqDiff(pkt.SequenceNumber, backRecoveredPacket.SequenceNumber) > uint16(maxMediaPackets) {
				d.logger.Infow("flexfec: big gap in media sequence numbers - resetting buffers")
				d.recoveredPackets = nil
				d.receivedFECPackets = nil
			}
		}
	}

	d.insertPacket(pkt)

	recovered := d.attemptRecovery()
	d.stats.PacketsRecovered += uint64(len(recovered))
	return recovered
}

func (d *Decoder) insertPacket(receivedPkt *rtp.Packet) {
	// Discard old FEC packets such that the sequence numbers in
	// `receivedFECPackets` span at most 1/2 of the sequence number space.
	// This is important for keeping `receivedFECPackets` sorted, and may
	// also reduce the possibility of incorrect decoding due to sequence
	// number wrap-around.
	if len(d.receivedFECPackets) > 0 && receivedPkt.SSRC == d.fecSSRC {
		toRemove := 0
		for _, fecPkt := range d.receivedFECPackets {
			if absInt(int(receivedPkt.SequenceNumber)-int(fecPkt.packet.SequenceNumber)) > 0x3fff {
				toRemove++
			} else {
				// no need to keep iterating, since receivedFECPackets is sorted
				break
			}
		}
		if toRemove > 0 {
			d.receivedFECPackets = d.receivedFECPackets[toRemove:]
		}
	}

	switch receivedPkt.SSRC {
	case d.fecSSRC:
		d.insertFECPacket(receivedPkt)
	case d.protectedSSRC:
		d.insertMediaPacket(receivedPkt)
	}

	d.discardOldRecoveredPackets()
}

func (d *Decoder) insertMediaPacket(receivedPkt *rtp.Packet) {
	for _, recoveredPacket := range d.recoveredPackets {
		if recoveredPacket.SequenceNumber == receivedPkt.SequenceNumber {
			return
		}
	}

	d.recoveredPackets = append(d.recoveredPackets, receivedPkt)
	sort.Slice(d.recoveredPackets, func(i, j int) bool {
		return isNewerSeq(d.recoveredPackets[i].SequenceNumber, d.recoveredPackets[j].SequenceNumber)
	})
	d.updateCoveringFecPackets(receivedPkt)
}

func (d *Decoder) updateCoveringFecPackets(receivedPkt *rtp.Packet) {
	for i := range d.receivedFECPackets {
		for _, pp := range d.receivedFECPackets[i].protectedPackets {
			if pp.seq == receivedPkt.SequenceNumber {
				pp.packet = receivedPkt
			}
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

	protectedSeqs := decodeMask(uint64(fec.mask0), fecMask0Bits, fec.seqNumBase)
	if fec.mask1 != 0 {
		protectedSeqs = append(protectedSeqs, decodeMask(uint64(fec.mask1), fecMask1Bits, fec.seqNumBase+fecMask0Bits)...)
	}
	if fec.mask2 != 0 {
		protectedSeqs = append(protectedSeqs, decodeMask(fec.mask2, fecMask2Bits, fec.seqNumBase+fecMask0Bits+fecMask1Bits)...)
	}

	if len(protectedSeqs) == 0 {
		d.stats.FECPacketsDiscarded++
		d.logger.Debugw("flexfec: discarding packet", "error", errEmptyMask)
		return
	}

	protectedPackets := make([]*protectedPacket, 0, len(protectedSeqs))
	protectedSeqIt := 0
	recoveredPacketIt := 0

	for protectedSeqIt < len(protectedSeqs) && recoveredPacketIt < len(d.recoveredPackets) {
		switch {
		case isNewerSeq(protectedSeqs[protectedSeqIt], d.recoveredPackets[recoveredPacketIt].SequenceNumber):
			protectedPackets = append(protectedPackets, &protectedPacket{
				seq:    protectedSeqs[protectedSeqIt],
				packet: nil,
			})
			protectedSeqIt++
		case isNewerSeq(d.recoveredPackets[recoveredPacketIt].SequenceNumber, protectedSeqs[protectedSeqIt]):
			recoveredPacketIt++
		default:
			protectedPackets = append(protectedPackets, &protectedPacket{
				seq:    protectedSeqs[protectedSeqIt],
				packet: d.recoveredPackets[recoveredPacketIt],
			})
			protectedSeqIt++
			recoveredPacketIt++
		}
	}

	for protectedSeqIt < len(protectedSeqs) {
		protectedPackets = append(protectedPackets, &protectedPacket{
			seq:    protectedSeqs[protectedSeqIt],
			packet: nil,
		})
		protectedSeqIt++
	}
	d.receivedFECPackets = append(d.receivedFECPackets, fecPacketState{
		packet:           fecPkt,
		flexFec:          fec,
		protectedPackets: protectedPackets,
	})

	sort.Slice(d.receivedFECPackets, func(i, j int) bool {
		return isNewerSeq(d.receivedFECPackets[i].packet.SequenceNumber, d.receivedFECPackets[j].packet.SequenceNumber)
	})

	if len(d.receivedFECPackets) > maxFECPackets {
		d.receivedFECPackets = d.receivedFECPackets[1:]
	}
}

func (d *Decoder) attemptRecovery() []*rtp.Packet {
	var recoveredPackets []*rtp.Packet
	for {
		packetsRecovered := 0
		for i := range d.receivedFECPackets {
			fecPkt := &d.receivedFECPackets[i]
			packetsMissing := 0
			for _, pkt := range fecPkt.protectedPackets {
				if pkt.packet == nil {
					packetsMissing++
					if packetsMissing > 1 {
						break
					}
				}
			}

			if packetsMissing != 1 {
				continue
			}

			recovered, err := d.recoverPacket(fecPkt)
			if err != nil {
				d.logger.Debugw("flexfec: failed to recover packet", "error", err)
				continue
			}

			recoveredPackets = append(recoveredPackets, recovered)
			d.recoveredPackets = append(d.recoveredPackets, recovered)
			sort.Slice(d.recoveredPackets, func(i, j int) bool {
				return isNewerSeq(d.recoveredPackets[i].SequenceNumber, d.recoveredPackets[j].SequenceNumber)
			})

			d.updateCoveringFecPackets(recovered)
			d.discardOldRecoveredPackets()
			packetsRecovered++
		}

		if packetsRecovered == 0 {
			break
		}
	}

	return recoveredPackets
}

func (d *Decoder) recoverPacket(fec *fecPacketState) (*rtp.Packet, error) {
	// https://datatracker.ietf.org/doc/html/draft-ietf-payload-flexible-fec-scheme-03#section-6.3.2

	// 2. For the repair packet in T, extract the FEC bit string as the
	//    first 80 bits of the FEC header.
	headerRecovery := make([]byte, 12)
	copy(headerRecovery, fec.packet.Payload[:10])

	var seqnum uint16
	for _, pp := range fec.protectedPackets {
		if pp.packet != nil {
			// 1.   For each of the source packets that are successfully received in
			//      T, compute the 80-bit string by concatenating the first 64 bits
			//      of their RTP header and the unsigned network-ordered 16-bit
			//      representation of their length in bytes minus 12.
			receivedHeader, err := pp.packet.Header.Marshal()
			if err != nil {
				return nil, fmt.Errorf("marshal received header: %w", err)
			}
			binary.BigEndian.PutUint16(receivedHeader[2:4], uint16(pp.packet.MarshalSize()-12))
			for i := 0; i < 8; i++ {
				headerRecovery[i] ^= receivedHeader[i]
			}
		} else {
			seqnum = pp.seq
		}
	}

	// set version to 2
	headerRecovery[0] |= 0x80
	headerRecovery[0] &= 0xbf
	payloadLength := binary.BigEndian.Uint16(headerRecovery[2:4])
	binary.BigEndian.PutUint16(headerRecovery[2:4], seqnum)
	binary.BigEndian.PutUint32(headerRecovery[8:12], d.protectedSSRC)

	payloadRecovery := make([]byte, payloadLength)
	copy(payloadRecovery, fec.flexFec.payload)
	for _, pp := range fec.protectedPackets {
		if pp.packet != nil {
			packet, err := pp.packet.Marshal()
			if err != nil {
				return nil, fmt.Errorf("marshal protected packet: %w", err)
			}
			for i := 0; i < min(int(payloadLength), len(packet)-12); i++ {
				payloadRecovery[i] ^= packet[12+i]
			}
		}
	}

	headerRecovery = append(headerRecovery, payloadRecovery...)

	packet := &rtp.Packet{}
	if err := packet.Unmarshal(headerRecovery); err != nil {
		return nil, fmt.Errorf("unmarshal recovered: %w", err)
	}

	return packet, nil
}

func (d *Decoder) discardOldRecoveredPackets() {
	if len(d.recoveredPackets) > recoveredPacketsLimit {
		d.recoveredPackets = d.recoveredPackets[len(d.recoveredPackets)-recoveredPacketsLimit:]
	}
}

func decodeMask(mask uint64, bitCount uint16, seqNumBase uint16) []uint16 {
	res := make([]uint16, 0)
	for i := uint16(0); i < bitCount; i++ {
		if (mask>>(bitCount-1-i))&1 == 1 {
			res = append(res, seqNumBase+i)
		}
	}

	return res
}

type fecPacketState struct {
	packet           *rtp.Packet
	flexFec          flexFec
	protectedPackets []*protectedPacket
}

type flexFec struct {
	protectedSSRC uint32
	seqNumBase    uint16
	mask0         uint16
	mask1         uint32
	mask2         uint64
	payload       []byte
}

type protectedPacket struct {
	seq    uint16
	packet *rtp.Packet
}

func parseFlexFEC03Header(data []byte) (flexFec, error) {
	if len(data) < 20 {
		return flexFec{}, fmt.Errorf("%w: length %d", errPacketTruncated, len(data))
	}

	rBit := (data[0] & fecRetransmissionBit) != 0
	if rBit {
		return flexFec{}, errRetransmissionBitSet
	}

	fBit := (data[0] & fecInflexibleBit) != 0
	if fBit {
		return flexFec{}, errInflexibleGeneratorMatrix
	}

	ssrcCount := data[8]
	if ssrcCount != 1 {
		return flexFec{}, fmt.Errorf("%w: count %d", errMultipleSSRCProtection, ssrcCount)
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
			return flexFec{}, fmt.Errorf("%w: length %d", errPacketTruncated, len(data))
		}

		kBit1 := (rawPacketMask[2] & fecMaskKBit) != 0
		maskPart1 = binary.BigEndian.Uint32(rawPacketMask[2:]) & fecMask1Value

		if kBit1 {
			payload = rawPacketMask[6:]
		} else {
			if len(data) < 32 {
				return flexFec{}, fmt.Errorf("%w: length %d", errPacketTruncated, len(data))
			}

			kBit2 := (rawPacketMask[6] & fecMaskKBit) != 0
			maskPart2 = binary.BigEndian.Uint64(rawPacketMask[6:]) & fecMask2Value

			if kBit2 {
				payload = rawPacketMask[14:]
			} else {
				return flexFec{}, errLastOptionalMaskKBitSetToFalse
			}
		}
	}

	return flexFec{
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

func absInt(x int) int {
	if x >= 0 {
		return x
	}

	return -x
}

func isNewerSeq(prevValue, value uint16) bool {
	// half-way mark
	breakpoint := uint16(0x8000)
	if value-prevValue == breakpoint {
		return value > prevValue
	}

	return value != prevValue && (value-prevValue) < breakpoint
}
