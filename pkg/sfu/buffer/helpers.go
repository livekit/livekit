package buffer

import (
	"encoding/binary"
	"errors"
	"sync/atomic"
)

var (
	errShortPacket   = errors.New("packet is not large enough")
	errNilPacket     = errors.New("invalid nil packet")
	errInvalidPacket = errors.New("invalid packet")
)

type atomicBool int32

func (a *atomicBool) set(value bool) {
	var i int32
	if value {
		i = 1
	}
	atomic.StoreInt32((*int32)(a), i)
}

func (a *atomicBool) get() bool {
	return atomic.LoadInt32((*int32)(a)) != 0
}

// VP8 is a helper to get temporal data from VP8 packet header
/*
	VP8 Payload Descriptor
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
type VP8 struct {
	TemporalSupported bool // LK-TODO: CLEANUP-REMOVE
	FirstByte         byte

	PictureIDPresent int
	PictureID        uint16 /* 8 or 16 bits, picture ID */
	PicIDIdx         int    // LK-TODO: CLEANUP-REMOVE
	MBit             bool

	TL0PICIDXPresent int
	TL0PICIDX        uint8 /* 8 bits temporal level zero index */
	TlzIdx           int   // LK-TODO: CLEANUP-REMOVE

	// Optional Header If either of the T or K bits are set to 1,
	// the TID/Y/KEYIDX extension field MUST be present.
	TIDPresent int
	TID        uint8 /* 2 bits temporal layer idx */
	Y          uint8

	KEYIDXPresent int
	KEYIDX        uint8 /* 5 bits of key frame idx */

	HeaderSize int

	// IsKeyFrame is a helper to detect if current packet is a keyframe
	IsKeyFrame bool
}

// Unmarshal parses the passed byte slice and stores the result in the VP8 this method is called upon
func (p *VP8) Unmarshal(payload []byte) error {
	if payload == nil {
		return errNilPacket
	}

	payloadLen := len(payload)

	if payloadLen < 1 {
		return errShortPacket
	}

	idx := 0
	p.FirstByte = payload[idx]
	S := payload[idx]&0x10 > 0
	// Check for extended bit control
	if payload[idx]&0x80 > 0 {
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		I := payload[idx]&0x80 > 0
		L := payload[idx]&0x40 > 0
		T := payload[idx]&0x20 > 0
		K := payload[idx]&0x10 > 0
		if L && !T {
			return errInvalidPacket
		}
		// Check if T is present, if not, no temporal layer is available
		p.TemporalSupported = payload[idx]&0x20 > 0
		// Check for PictureID
		if I {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			p.PicIDIdx = idx
			p.PictureIDPresent = 1
			pid := payload[idx] & 0x7f
			// Check if m is 1, then Picture ID is 15 bits
			if payload[idx]&0x80 > 0 {
				idx++
				if payloadLen < idx+1 {
					return errShortPacket
				}
				p.MBit = true
				p.PictureID = binary.BigEndian.Uint16([]byte{pid, payload[idx]})
			} else {
				p.PictureID = uint16(pid)
			}
		}
		// Check if TL0PICIDX is present
		if L {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			p.TlzIdx = idx
			p.TL0PICIDXPresent = 1

			if int(idx) >= payloadLen {
				return errShortPacket
			}
			p.TL0PICIDX = payload[idx]
		}
		if T || K {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			if T {
				p.TIDPresent = 1
				p.TID = (payload[idx] & 0xc0) >> 6
				p.Y = (payload[idx] & 0x20) >> 5
			}
			if K {
				p.KEYIDXPresent = 1
				p.KEYIDX = payload[idx] & 0x1f
			}
		}
		if idx >= payloadLen {
			return errShortPacket
		}
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		// Check is packet is a keyframe by looking at P bit in vp8 payload
		p.IsKeyFrame = payload[idx]&0x01 == 0 && S
	} else {
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		// Check is packet is a keyframe by looking at P bit in vp8 payload
		p.IsKeyFrame = payload[idx]&0x01 == 0 && S
	}
	p.HeaderSize = idx
	return nil
}

func (v *VP8) MarshalTo(buf []byte) error {
	if len(buf) < v.HeaderSize {
		return errShortPacket
	}

	idx := 0
	buf[idx] = v.FirstByte
	if (v.PictureIDPresent + v.TL0PICIDXPresent + v.TIDPresent + v.KEYIDXPresent) != 0 {
		buf[idx] |= 0x80 // X bit
		idx++
		buf[idx] = byte(v.PictureIDPresent<<7) | byte(v.TL0PICIDXPresent<<6) | byte(v.TIDPresent<<5) | byte(v.KEYIDXPresent<<4)
		idx++
		if v.PictureIDPresent == 1 {
			if v.MBit {
				buf[idx] = 0x80 | byte((v.PictureID>>8)&0x7f)
				buf[idx+1] = byte(v.PictureID & 0xff)
				idx += 2
			} else {
				buf[idx] = byte(v.PictureID)
				idx++
			}
		}
		if v.TL0PICIDXPresent == 1 {
			buf[idx] = byte(v.TL0PICIDX)
			idx++
		}
		if v.TIDPresent == 1 || v.KEYIDXPresent == 1 {
			buf[idx] = 0
			if v.TIDPresent == 1 {
				buf[idx] = byte(v.TID<<6) | byte(v.Y<<5)
			}
			if v.KEYIDXPresent == 1 {
				buf[idx] |= byte(v.KEYIDX & 0x1f)
			}
			idx++
		}
	} else {
		buf[idx] &^= 0x80 // X bit
		idx++
	}

	return nil
}

func VP8PictureIdSizeDiff(mBit1 bool, mBit2 bool) int {
	if mBit1 == mBit2 {
		return 0
	}

	if mBit1 {
		return 1
	}

	return -1
}

// isH264Keyframe detects if h264 payload is a keyframe
// this code was taken from https://github.com/jech/galene/blob/codecs/rtpconn/rtpreader.go#L45
// all credits belongs to Juliusz Chroboczek @jech and the awesome Galene SFU
func isH264Keyframe(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	nalu := payload[0] & 0x1F
	if nalu == 0 {
		// reserved
		return false
	} else if nalu <= 23 {
		// simple NALU
		return nalu == 5
	} else if nalu == 24 || nalu == 25 || nalu == 26 || nalu == 27 {
		// STAP-A, STAP-B, MTAP16 or MTAP24
		i := 1
		if nalu == 25 || nalu == 26 || nalu == 27 {
			// skip DON
			i += 2
		}
		for i < len(payload) {
			if i+2 > len(payload) {
				return false
			}
			length := uint16(payload[i])<<8 |
				uint16(payload[i+1])
			i += 2
			if i+int(length) > len(payload) {
				return false
			}
			offset := 0
			if nalu == 26 {
				offset = 3
			} else if nalu == 27 {
				offset = 4
			}
			if offset >= int(length) {
				return false
			}
			n := payload[i+offset] & 0x1F
			if n == 7 {
				return true
			} else if n >= 24 {
				// is this legal?
				Logger.V(0).Info("Non-simple NALU within a STAP")
			}
			i += int(length)
		}
		if i == len(payload) {
			return false
		}
		return false
	} else if nalu == 28 || nalu == 29 {
		// FU-A or FU-B
		if len(payload) < 2 {
			return false
		}
		if (payload[1] & 0x80) == 0 {
			// not a starting fragment
			return false
		}
		return payload[1]&0x1F == 7
	}
	return false
}
