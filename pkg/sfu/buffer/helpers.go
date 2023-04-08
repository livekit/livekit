package buffer

import (
	"encoding/binary"
	"errors"

	"github.com/livekit/protocol/logger"
)

var (
	errShortPacket   = errors.New("packet is not large enough")
	errNilPacket     = errors.New("invalid nil packet")
	errInvalidPacket = errors.New("invalid packet")
)

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
	FirstByte byte
	S         bool

	I         bool
	M         bool
	PictureID uint16 /* 8 or 16 bits, picture ID */

	L         bool
	TL0PICIDX uint8 /* 8 bits temporal level zero index */

	// Optional Header If either of the T or K bits are set to 1,
	// the TID/Y/KEYIDX extension field MUST be present.
	T   bool
	TID uint8 /* 2 bits temporal layer idx */
	Y   bool

	K      bool
	KEYIDX uint8 /* 5 bits of key frame idx */

	HeaderSize int

	// IsKeyFrame is a helper to detect if current packet is a keyframe
	IsKeyFrame bool
}

// Unmarshal parses the passed byte slice and stores the result in the VP8 this method is called upon
func (v *VP8) Unmarshal(payload []byte) error {
	if payload == nil {
		return errNilPacket
	}

	payloadLen := len(payload)
	if payloadLen < 1 {
		return errShortPacket
	}

	idx := 0
	v.FirstByte = payload[idx]
	v.S = payload[idx]&0x10 > 0
	// Check for extended bit control
	if payload[idx]&0x80 > 0 {
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		v.I = payload[idx]&0x80 > 0
		v.L = payload[idx]&0x40 > 0
		v.T = payload[idx]&0x20 > 0
		v.K = payload[idx]&0x10 > 0
		if v.L && !v.T {
			return errInvalidPacket
		}

		if v.I {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			pid := payload[idx] & 0x7f
			// if m is 1, then Picture ID is 15 bits
			v.M = payload[idx]&0x80 > 0
			if v.M {
				idx++
				if payloadLen < idx+1 {
					return errShortPacket
				}
				v.PictureID = binary.BigEndian.Uint16([]byte{pid, payload[idx]})
			} else {
				v.PictureID = uint16(pid)
			}
		}

		if v.L {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			v.TL0PICIDX = payload[idx]
		}

		if v.T || v.K {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}

			if v.T {
				v.TID = (payload[idx] & 0xc0) >> 6
				v.Y = (payload[idx] & 0x20) > 0
			}

			if v.K {
				v.KEYIDX = payload[idx] & 0x1f
			}
		}
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}

		// Check is packet is a keyframe by looking at P bit in vp8 payload
		v.IsKeyFrame = payload[idx]&0x01 == 0 && v.S
	} else {
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		// Check is packet is a keyframe by looking at P bit in vp8 payload
		v.IsKeyFrame = payload[idx]&0x01 == 0 && v.S
	}
	v.HeaderSize = idx
	return nil
}

func (v *VP8) Marshal() ([]byte, error) {
	buf := make([]byte, v.HeaderSize)
	err := v.MarshalTo(buf)
	return buf, err
}

func (v *VP8) MarshalTo(buf []byte) error {
	if len(buf) < v.HeaderSize {
		return errShortPacket
	}

	idx := 0
	buf[idx] = v.FirstByte
	if v.I || v.L || v.T || v.K {
		buf[idx] |= 0x80 // X bit
		idx++

		xpos := idx
		xval := byte(0)

		idx++
		if v.I {
			xval |= (1 << 7)
			if v.M {
				buf[idx] = 0x80 | byte((v.PictureID>>8)&0x7f)
				buf[idx+1] = byte(v.PictureID & 0xff)
				idx += 2
			} else {
				buf[idx] = byte(v.PictureID)
				idx++
			}
		}

		if v.L {
			xval |= (1 << 6)
			buf[idx] = v.TL0PICIDX
			idx++
		}

		if v.T || v.K {
			buf[idx] = 0
			if v.T {
				xval |= (1 << 5)
				buf[idx] = v.TID << 6
				if v.Y {
					buf[idx] |= (1 << 5)
				}
			}

			if v.K {
				xval |= (1 << 4)
				buf[idx] |= v.KEYIDX & 0x1f
			}
			idx++
		}

		buf[xpos] = xval
	} else {
		buf[idx] &^= 0x80 // X bit
		idx++
	}

	return nil
}

// -------------------------------------

func VPxPictureIdSizeDiff(mBit1 bool, mBit2 bool) int {
	if mBit1 == mBit2 {
		return 0
	}

	if mBit1 {
		return 1
	}

	return -1
}

// -------------------------------------

// IsH264KeyFrame detects if h264 payload is a keyframe
// this code was taken from https://github.com/jech/galene/blob/codecs/rtpconn/rtpreader.go#L45
// all credits belongs to Juliusz Chroboczek @jech and the awesome Galene SFU
func IsH264KeyFrame(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	nalu := payload[0] & 0x1F
	if nalu == 0 {
		// reserved
		return false
	} else if nalu <= 23 {
		// simple NALU
		return nalu == 7
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
				logger.Debugw("Non-simple NALU within a STAP")
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

// -------------------------------------

func IsVP9KeyFrame(payload []byte) bool {
	payloadLen := len(payload)
	if payloadLen < 1 {
		return false
	}

	idx := 0
	I := payload[idx]&0x80 > 0
	P := payload[idx]&0x40 > 0
	L := payload[idx]&0x20 > 0
	F := payload[idx]&0x10 > 0
	B := payload[idx]&0x08 > 0

	if F && !I {
		return false
	}

	// Check for PictureID
	if I {
		idx++
		if payloadLen < idx+1 {
			return false
		}
		// Check if m is 1, then Picture ID is 15 bits
		if payload[idx]&0x80 > 0 {
			idx++
			if payloadLen < idx+1 {
				return false
			}
		}
	}

	// Check if TL0PICIDX is present
	sid := -1
	if L {
		idx++
		if payloadLen < idx+1 {
			return false
		}

		tid := (payload[idx] >> 5) & 0x7
		if !P && tid != 0 {
			return false
		}

		sid = int((payload[idx] >> 1) & 0x7)
	}

	return !P && (!L || (L && sid == 0)) && B
}

// -------------------------------------

// IsAV1KeyFrame detects if av1 payload is a keyframe
// taken from https://github.com/jech/galene/blob/master/codecs/codecs.go
// all credits belongs to Juliusz Chroboczek @jech and the awesome Galene SFU
func IsAV1KeyFrame(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	// Z=0, N=1
	if (payload[0] & 0x88) != 0x08 {
		return false
	}
	w := (payload[0] & 0x30) >> 4

	getObu := func(data []byte, last bool) ([]byte, int, bool) {
		if last {
			return data, len(data), false
		}
		offset := 0
		length := 0
		for {
			if len(data) <= offset {
				return nil, offset, offset > 0
			}
			l := data[offset]
			length |= int(l&0x7f) << (offset * 7)
			offset++
			if (l & 0x80) == 0 {
				break
			}
		}
		if len(data) < offset+length {
			return data[offset:], len(data), true
		}
		return data[offset : offset+length],
			offset + length, false
	}
	offset := 1
	i := 0
	for {
		obu, length, truncated :=
			getObu(payload[offset:], int(w) == i+1)
		if len(obu) < 1 {
			return false
		}
		tpe := (obu[0] & 0x38) >> 3
		switch i {
		case 0:
			// OBU_SEQUENCE_HEADER
			if tpe != 1 {
				return false
			}
		default:
			// OBU_FRAME_HEADER or OBU_FRAME
			if tpe == 3 || tpe == 6 {
				if len(obu) < 2 {
					return false
				}
				// show_existing_frame == 0
				if (obu[1] & 0x80) != 0 {
					return false
				}
				// frame_type == KEY_FRAME
				return (obu[1] & 0x60) == 0
			}
		}
		if truncated || i >= int(w) {
			// the first frame header is in a second
			// packet, give up.
			return false
		}
		offset += length
		i++
	}
}

// -------------------------------------
