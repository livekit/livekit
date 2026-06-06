// Copyright 2024 LiveKit, Inc.
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

// Package flexfec implements a FlexFEC-03 decoder used by the SFU to recover
// RTP packets lost on the publisher -> SFU path.
//
// FlexFEC (RFC 8627 / draft-ietf-payload-flexible-fec-scheme-03) sends repair
// packets on a dedicated SSRC. Each repair packet references the protected
// source packets via a base sequence number plus a bitmask and carries the XOR
// of the protected packets' headers and payloads. When a single protected
// packet is missing, it can be reconstructed by XOR-ing the repair packet with
// the other protected source packets.
//
// This is a port of pion/interceptor's (unexported) FlexFEC-03 decoder, adapted
// to be reusable from the SFU buffer layer. The decoder is NOT safe for
// concurrent use; the caller must serialize access (the SFU feeds it from a
// single SRTP read goroutine).
package flexfec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/pion/rtp"
)

var (
	errPacketTruncated                = errors.New("packet truncated")
	errRetransmissionBitSet           = errors.New("packet with retransmission bit set not supported")
	errInflexibleGeneratorMatrix      = errors.New("packet with inflexible generator matrix not supported")
	errMultipleSSRCProtection         = errors.New("multiple ssrc protection not supported")
	errLastOptionalMaskKBitSetToFalse = errors.New("k-bit of last optional mask is set to false")
)

const (
	defaultMaxMediaPackets = 100
	defaultMaxFECPackets   = 100
	recoveredPacketLimit   = 192
)

// Decoder reconstructs lost source RTP packets from a FlexFEC-03 repair stream.
//
// Feed it both the received source packets (SSRC == ProtectedSSRC) and the
// received repair packets (SSRC == SSRC) via Decode. Each call returns any
// source packets recovered as a result of the newly inserted packet.
type Decoder struct {
	ssrc                uint32
	protectedStreamSSRC uint32
	maxMediaPackets     int
	maxFECPackets       int
	recoveredPackets    []rtp.Packet
	receivedFECPackets  []fecPacketState
}

// NewDecoder creates a decoder for a single (repair SSRC, protected SSRC) pair.
func NewDecoder(fecSSRC uint32, protectedStreamSSRC uint32) *Decoder {
	return &Decoder{
		ssrc:                fecSSRC,
		protectedStreamSSRC: protectedStreamSSRC,
		maxMediaPackets:     defaultMaxMediaPackets,
		maxFECPackets:       defaultMaxFECPackets,
		recoveredPackets:    make([]rtp.Packet, 0),
		receivedFECPackets:  make([]fecPacketState, 0),
	}
}

// SSRC returns the FlexFEC repair stream SSRC this decoder handles.
func (d *Decoder) SSRC() uint32 {
	return d.ssrc
}

// ProtectedSSRC returns the source stream SSRC this decoder protects.
func (d *Decoder) ProtectedSSRC() uint32 {
	return d.protectedStreamSSRC
}

// Decode inserts a received packet (either a source packet on ProtectedSSRC or
// a repair packet on SSRC) and returns any source packets recovered as a
// result. The supplied packet is cloned, so the caller may reuse the backing
// buffer after Decode returns.
func (d *Decoder) Decode(receivedPacket rtp.Packet) []rtp.Packet {
	if receivedPacket.SSRC != d.ssrc && receivedPacket.SSRC != d.protectedStreamSSRC {
		return nil
	}

	pkt := clonePacket(receivedPacket)

	if len(d.recoveredPackets) == d.maxMediaPackets {
		backRecoveredPacket := d.recoveredPackets[len(d.recoveredPackets)-1]
		if backRecoveredPacket.SSRC == pkt.SSRC {
			if seqDiff(pkt.SequenceNumber, backRecoveredPacket.SequenceNumber) > uint16(d.maxMediaPackets) {
				d.recoveredPackets = nil
				d.receivedFECPackets = nil
			}
		}
	}

	d.insertPacket(pkt)

	return d.attemptRecovery()
}

func (d *Decoder) insertPacket(receivedPkt rtp.Packet) {
	// Discard old FEC packets such that the sequence numbers in receivedFECPackets
	// span at most 1/2 of the sequence number space. This keeps the slice sorted
	// and reduces incorrect decoding due to sequence number wrap-around.
	if len(d.receivedFECPackets) > 0 && receivedPkt.SSRC == d.ssrc {
		toRemove := 0
		for _, fecPkt := range d.receivedFECPackets {
			if abs(int(receivedPkt.SequenceNumber)-int(fecPkt.packet.SequenceNumber)) > 0x3fff {
				toRemove++
			} else {
				break
			}
		}
		if toRemove > 0 {
			d.receivedFECPackets = d.receivedFECPackets[toRemove:]
		}
	}

	switch receivedPkt.SSRC {
	case d.ssrc:
		d.insertFECPacket(receivedPkt)
	case d.protectedStreamSSRC:
		d.insertMediaPacket(receivedPkt)
	}

	d.discardOldRecoveredPackets()
}

func (d *Decoder) insertMediaPacket(receivedPkt rtp.Packet) {
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

func (d *Decoder) updateCoveringFecPackets(receivedPkt rtp.Packet) {
	pkt := receivedPkt
	for _, fecPkt := range d.receivedFECPackets {
		for _, protectedPacket := range fecPkt.protectedPackets {
			if protectedPacket.seq == pkt.SequenceNumber {
				protectedPacket.packet = &pkt
			}
		}
	}
}

func (d *Decoder) insertFECPacket(fecPkt rtp.Packet) {
	for _, existingFECPacket := range d.receivedFECPackets {
		if existingFECPacket.packet.SequenceNumber == fecPkt.SequenceNumber {
			return
		}
	}

	fec, err := parseFlexFEC03Header(fecPkt.Payload)
	if err != nil {
		return
	}

	if fec.protectedSSRC != d.protectedStreamSSRC {
		return
	}

	protectedSeqs := decodeMask(uint64(fec.mask0), 15, fec.seqNumBase)
	if fec.mask1 != 0 {
		protectedSeqs = append(protectedSeqs, decodeMask(uint64(fec.mask1), 31, fec.seqNumBase+15)...)
	}
	if fec.mask2 != 0 {
		protectedSeqs = append(protectedSeqs, decodeMask(fec.mask2, 63, fec.seqNumBase+46)...)
	}

	if len(protectedSeqs) == 0 {
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
				packet: &d.recoveredPackets[recoveredPacketIt],
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

	if len(d.receivedFECPackets) > d.maxFECPackets {
		d.receivedFECPackets = d.receivedFECPackets[1:]
	}
}

func (d *Decoder) attemptRecovery() []rtp.Packet {
	recoveredPackets := make([]rtp.Packet, 0)
	for {
		packetsRecovered := 0
		for i := range d.receivedFECPackets {
			fecPkt := d.receivedFECPackets[i]
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

			recovered, err := d.recoverPacket(&fecPkt)
			if err != nil {
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

func (d *Decoder) recoverPacket(fec *fecPacketState) (rtp.Packet, error) {
	// https://datatracker.ietf.org/doc/html/draft-ietf-payload-flexible-fec-scheme-03#section-6.3.2

	// extract the FEC bit string as the first 80 bits of the FEC header.
	headerRecovery := make([]byte, 12)
	if len(fec.packet.Payload) < 10 {
		return rtp.Packet{}, errPacketTruncated
	}
	copy(headerRecovery, fec.packet.Payload[:10])

	var seqnum uint16
	for _, protectedPacket := range fec.protectedPackets {
		if protectedPacket.packet != nil {
			// for each received source packet, compute the 80-bit string by
			// concatenating the first 64 bits of its RTP header and the 16-bit
			// network-ordered representation of its length in bytes minus 12.
			receivedHeader, err := protectedPacket.packet.Header.Marshal()
			if err != nil {
				return rtp.Packet{}, fmt.Errorf("marshal received header: %w", err)
			}
			binary.BigEndian.PutUint16(receivedHeader[2:4], uint16(protectedPacket.packet.MarshalSize()-12))
			for i := range 8 {
				headerRecovery[i] ^= receivedHeader[i]
			}
		} else {
			seqnum = protectedPacket.seq
		}
	}

	// set version to 2
	headerRecovery[0] |= 0x80
	headerRecovery[0] &= 0xbf
	payloadLength := binary.BigEndian.Uint16(headerRecovery[2:4])
	binary.BigEndian.PutUint16(headerRecovery[2:4], seqnum)
	binary.BigEndian.PutUint32(headerRecovery[8:12], d.protectedStreamSSRC)

	payloadRecovery := make([]byte, payloadLength)
	copy(payloadRecovery, fec.flexFec.payload)
	for _, protectedPacket := range fec.protectedPackets {
		if protectedPacket.packet != nil {
			packet, err := protectedPacket.packet.Marshal()
			if err != nil {
				return rtp.Packet{}, fmt.Errorf("marshal protected packet: %w", err)
			}
			for i := 0; i < min(int(payloadLength), len(packet)-12); i++ {
				payloadRecovery[i] ^= packet[12+i]
			}
		}
	}

	headerRecovery = append(headerRecovery, payloadRecovery...)

	var packet rtp.Packet
	if err := packet.Unmarshal(headerRecovery); err != nil {
		return rtp.Packet{}, fmt.Errorf("unmarshal recovered: %w", err)
	}

	return packet, nil
}

func (d *Decoder) discardOldRecoveredPackets() {
	if len(d.recoveredPackets) > recoveredPacketLimit {
		d.recoveredPackets = d.recoveredPackets[len(d.recoveredPackets)-recoveredPacketLimit:]
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
	packet           rtp.Packet
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

	rBit := (data[0] & 0x80) != 0
	if rBit {
		return flexFec{}, errRetransmissionBitSet
	}

	fBit := (data[0] & 0x40) != 0
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

	kBit0 := (rawPacketMask[0] & 0x80) != 0
	maskPart0 := binary.BigEndian.Uint16(rawPacketMask[0:2]) & 0x7FFF
	var maskPart1 uint32
	var maskPart2 uint64

	if kBit0 {
		payload = rawPacketMask[2:]
	} else {
		if len(data) < 24 {
			return flexFec{}, fmt.Errorf("%w: length %d", errPacketTruncated, len(data))
		}

		kBit1 := (rawPacketMask[2] & 0x80) != 0
		maskPart1 = binary.BigEndian.Uint32(rawPacketMask[2:]) & 0x7FFFFFFF

		if kBit1 {
			payload = rawPacketMask[6:]
		} else {
			if len(data) < 32 {
				return flexFec{}, fmt.Errorf("%w: length %d", errPacketTruncated, len(data))
			}

			kBit2 := (rawPacketMask[6] & 0x80) != 0
			maskPart2 = binary.BigEndian.Uint64(rawPacketMask[6:]) & 0x7FFFFFFFFFFFFFFF

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

func clonePacket(pkt rtp.Packet) rtp.Packet {
	cloned := pkt
	cloned.Header = pkt.Header.Clone()
	if pkt.Payload != nil {
		cloned.Payload = make([]byte, len(pkt.Payload))
		copy(cloned.Payload, pkt.Payload)
	}
	return cloned
}

func seqDiff(a, b uint16) uint16 {
	return min(a-b, b-a)
}

func abs(x int) int {
	if x >= 0 {
		return x
	}

	return -x
}

func isNewerSeq(prevValue, value uint16) bool {
	breakpoint := uint16(0x8000)
	if value-prevValue == breakpoint {
		return value > prevValue
	}

	return value != prevValue && (value-prevValue) < breakpoint
}
