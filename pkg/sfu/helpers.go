package sfu

import (
	"encoding/binary"
	"sync/atomic"
)

const ntpEpoch = 2208988800

type atomicBool struct {
	val int32
}

func (b *atomicBool) set(value bool) { // nolint: unparam
	var i int32
	if value {
		i = 1
	}

	atomic.StoreInt32(&(b.val), i)
}

func (b *atomicBool) get() bool {
	return atomic.LoadInt32(&(b.val)) != 0
}

// VP8Helper is a helper to get temporal data from VP8 packet header
/*
	VP8Helper Payload Descriptor
			0 1 2 3 4 5 6 7                      0 1 2 3 4 5 6 7
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
			|X|R|N|S|R| PID | (REQUIRED)        |X|R|N|S|R| PID | (REQUIRED)
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		X:  |I|L|T|K| RSV   | (OPTIONAL)   X:   |I|L|T|K| RSV   | (OPTIONAL)
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		I:  |M| PictureID   | (OPTIONAL)   I:   |M| PictureID   | (OPTIONAL)
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		L:  |   TL0PICIDX   | (OPTIONAL)        |   PictureID   |
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		T/K:|TID|Y| KEYIDX  | (OPTIONAL)   L:   |   TL0PICIDX   | (OPTIONAL)
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		T/K:|TID|Y| KEYIDX  | (OPTIONAL)
			+-+-+-+-+-+-+-+-+
*/
type VP8Helper struct {
	TemporalSupported bool
	// Optional Header
	PictureID uint16 /* 8 or 16 bits, picture ID */
	picIDIdx  uint8
	mBit      bool
	TL0PICIDX uint8 /* 8 bits temporal level zero index */
	tlzIdx    uint8

	// Optional Header If either of the T or K bits are set to 1,
	// the TID/Y/KEYIDX extension field MUST be present.
	TID uint8 /* 2 bits temporal layer idx*/
	// IsKeyFrame is a helper to detect if current packet is a keyframe
	IsKeyFrame bool
}

// Unmarshal parses the passed byte slice and stores the result in the VP8Helper this method is called upon
func (p *VP8Helper) Unmarshal(payload []byte) error {
	if payload == nil {
		return errNilPacket
	}

	payloadLen := len(payload)

	if payloadLen < 4 {
		return errShortPacket
	}

	var idx uint8
	// Check for extended bit control
	if payload[idx]&0x80 > 0 {
		idx++
		// Check if T is present, if not, no temporal layer is available
		p.TemporalSupported = payload[idx]&0x20 > 0
		L := payload[idx]&0x40 > 0
		// Check for PictureID
		if payload[idx]&0x80 > 0 {
			idx++
			p.picIDIdx = idx
			pid := payload[idx] & 0x7f
			// Check if m is 1, then Picture ID is 15 bits
			if payload[idx]&0x80 > 0 {
				idx++
				p.mBit = true
				p.PictureID = binary.BigEndian.Uint16([]byte{pid, payload[idx]})
			} else {
				p.PictureID = uint16(pid)
			}
		}
		// Check if TL0PICIDX is present
		if L {
			idx++
			p.tlzIdx = idx
			p.TL0PICIDX = payload[idx]
		}
		idx++
		// Set TID
		p.TID = (payload[idx] & 0xc0) >> 6
		idx++
		if int(idx) >= payloadLen {
			return errShortPacket
		}
		// Check is packet is a keyframe by looking at P bit in vp8 payload
		p.IsKeyFrame = payload[idx]&0x01 == 0
	}
	return nil
}

func snDiff(sn1, sn2 uint16) int {
	if sn1 == sn2 {
		return 0
	}
	if ((sn2 - sn1) & 0x8000) != 0 {
		return 1
	}
	return -1
}

func timeToNtp(ns int64) uint64 {
	seconds := uint64(ns/1e9 + ntpEpoch)
	fraction := uint64(((ns % 1e9) << 32) / 1e9)
	return seconds<<32 | fraction
}

// fromNtp converts a NTP timestamp into GO time
func fromNtp(seconds, fraction uint32) (tm int64) {
	n := (int64(fraction) * 1e9) >> 32
	tm = (int64(seconds)-ntpEpoch)*1e9 + n
	return
}
