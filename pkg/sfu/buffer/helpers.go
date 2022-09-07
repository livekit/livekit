package buffer

import (
	"encoding/binary"
	"errors"
	"time"

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

	PictureIDPresent int
	PictureID        uint16 /* 8 or 16 bits, picture ID */
	MBit             bool

	TL0PICIDXPresent int
	TL0PICIDX        uint8 /* 8 bits temporal level zero index */

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
		// Check for PictureID
		if I {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			v.PictureIDPresent = 1
			pid := payload[idx] & 0x7f
			// Check if m is 1, then Picture ID is 15 bits
			if payload[idx]&0x80 > 0 {
				idx++
				if payloadLen < idx+1 {
					return errShortPacket
				}
				v.MBit = true
				v.PictureID = binary.BigEndian.Uint16([]byte{pid, payload[idx]})
			} else {
				v.PictureID = uint16(pid)
			}
		}
		// Check if TL0PICIDX is present
		if L {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			v.TL0PICIDXPresent = 1

			if idx >= payloadLen {
				return errShortPacket
			}
			v.TL0PICIDX = payload[idx]
		}
		if T || K {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			if T {
				v.TIDPresent = 1
				v.TID = (payload[idx] & 0xc0) >> 6
				v.Y = (payload[idx] & 0x20) >> 5
			}
			if K {
				v.KEYIDXPresent = 1
				v.KEYIDX = payload[idx] & 0x1f
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
		v.IsKeyFrame = payload[idx]&0x01 == 0 && S
	} else {
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		// Check is packet is a keyframe by looking at P bit in vp8 payload
		v.IsKeyFrame = payload[idx]&0x01 == 0 && S
	}
	v.HeaderSize = idx
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
			buf[idx] = v.TL0PICIDX
			idx++
		}
		if v.TIDPresent == 1 || v.KEYIDXPresent == 1 {
			buf[idx] = 0
			if v.TIDPresent == 1 {
				buf[idx] = v.TID<<6 | v.Y<<5
			}
			if v.KEYIDXPresent == 1 {
				buf[idx] |= v.KEYIDX & 0x1f
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

// IsH264Keyframe detects if h264 payload is a keyframe
// this code was taken from https://github.com/jech/galene/blob/codecs/rtpconn/rtpreader.go#L45
// all credits belongs to Juliusz Chroboczek @jech and the awesome Galene SFU
func IsH264Keyframe(payload []byte) bool {
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

// IsAV1Keyframe detects if av1 payload is a keyframe
// taken from https://github.com/jech/galene/blob/master/codecs/codecs.go
// all credits belongs to Juliusz Chroboczek @jech and the awesome Galene SFU
func IsAV1Keyframe(payload []byte) bool {
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

var (
	ntpEpoch = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
)

type NtpTime uint64

func (t NtpTime) Duration() time.Duration {
	sec := (t >> 32) * 1e9
	frac := (t & 0xffffffff) * 1e9
	nsec := frac >> 32
	if uint32(frac) >= 0x80000000 {
		nsec++
	}
	return time.Duration(sec + nsec)
}

func (t NtpTime) Time() time.Time {
	return ntpEpoch.Add(t.Duration())
}

func ToNtpTime(t time.Time) NtpTime {
	nsec := uint64(t.Sub(ntpEpoch))
	sec := nsec / 1e9
	nsec = (nsec - sec*1e9) << 32
	frac := nsec / 1e9
	if nsec%1e9 >= 1e9/2 {
		frac++
	}
	return NtpTime(sec<<32 | frac)
}

// ------------------------------------------
