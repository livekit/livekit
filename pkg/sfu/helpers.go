package sfu

import (
	"encoding/binary"
	"strings"
	"time"

	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/webrtc/v3"
)

var (
	ntpEpoch = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
)

type ntpTime uint64

// LK-TODO: CLEANUP-REMOVE-FUNCTION
// setVp8TemporalLayer is a helper to detect and modify accordingly the vp8 payload to reflect
// temporal changes in the SFU.
// VP8 temporal layers implemented according https://tools.ietf.org/html/rfc7741
func setVP8TemporalLayer(p *buffer.ExtPacket, d *DownTrack) (buf []byte, picID uint16, tlz0Idx uint8, drop bool) {
	pkt, ok := p.Payload.(buffer.VP8)
	if !ok {
		return
	}

	layer := d.temporalLayer.get()
	currentLayer := uint16(layer)
	currentTargetLayer := uint16(layer >> 16)
	// Check if temporal getLayer is requested
	if currentTargetLayer != currentLayer {
		if pkt.TID <= uint8(currentTargetLayer) {
			d.temporalLayer.set(int32(currentTargetLayer)<<16 | int32(currentTargetLayer))
		}
	} else if pkt.TID > uint8(currentLayer) {
		drop = true
		return
	}

	buf = *d.payload
	buf = buf[:len(p.Packet.Payload)]
	copy(buf, p.Packet.Payload)

	picID = pkt.PictureID - d.simulcast.refPicID.get() + d.simulcast.pRefPicID.get() + 1
	tlz0Idx = pkt.TL0PICIDX - d.simulcast.refTlZIdx.get() + d.simulcast.pRefTlZIdx.get() + 1

	if p.Head {
		d.simulcast.lPicID.set(picID)
		d.simulcast.lTlZIdx.set(tlz0Idx)
	}

	modifyVP8TemporalPayload(buf, pkt.PicIDIdx, pkt.TlzIdx, picID, tlz0Idx, pkt.MBit)

	return
}

// LK-TODO: CLEANUP-REMOVE-FUNCTION
func modifyVP8TemporalPayload(payload []byte, picIDIdx, tlz0Idx int, picID uint16, tlz0ID uint8, mBit bool) {
	pid := make([]byte, 2)
	binary.BigEndian.PutUint16(pid, picID)
	payload[picIDIdx] = pid[0]
	if mBit {
		payload[picIDIdx] |= 0x80
		payload[picIDIdx+1] = pid[1]
	}
	payload[tlz0Idx] = tlz0ID
}

// Do a fuzzy find for a codec in the list of codecs
// Used for lookup up a codec in an existing list to find a match
func codecParametersFuzzySearch(needle webrtc.RTPCodecParameters, haystack []webrtc.RTPCodecParameters) (webrtc.RTPCodecParameters, error) {
	// First attempt to match on MimeType + SDPFmtpLine
	for _, c := range haystack {
		if strings.EqualFold(c.RTPCodecCapability.MimeType, needle.RTPCodecCapability.MimeType) &&
			c.RTPCodecCapability.SDPFmtpLine == needle.RTPCodecCapability.SDPFmtpLine {
			return c, nil
		}
	}

	// Fallback to just MimeType
	for _, c := range haystack {
		if strings.EqualFold(c.RTPCodecCapability.MimeType, needle.RTPCodecCapability.MimeType) {
			return c, nil
		}
	}

	return webrtc.RTPCodecParameters{}, webrtc.ErrCodecNotFound
}

func ntpToMillisSinceEpoch(ntp uint64) uint64 {
	// ntp time since epoch calculate fractional ntp as milliseconds
	// (lower 32 bits stored as 1/2^32 seconds) and add
	// ntp seconds (stored in higher 32 bits) as milliseconds
	return (((ntp & 0xFFFFFFFF) * 1000) >> 32) + ((ntp >> 32) * 1000)
}

func fastForwardTimestampAmount(newestTimestamp uint32, referenceTimestamp uint32) uint32 {
	if buffer.IsTimestampWrapAround(newestTimestamp, referenceTimestamp) {
		return uint32(uint64(newestTimestamp) + 0x100000000 - uint64(referenceTimestamp))
	}
	if newestTimestamp < referenceTimestamp {
		return 0
	}
	return newestTimestamp - referenceTimestamp
}

func (t ntpTime) Duration() time.Duration {
	sec := (t >> 32) * 1e9
	frac := (t & 0xffffffff) * 1e9
	nsec := frac >> 32
	if uint32(frac) >= 0x80000000 {
		nsec++
	}
	return time.Duration(sec + nsec)
}

func (t ntpTime) Time() time.Time {
	return ntpEpoch.Add(t.Duration())
}

func toNtpTime(t time.Time) ntpTime {
	nsec := uint64(t.Sub(ntpEpoch))
	sec := nsec / 1e9
	nsec = (nsec - sec*1e9) << 32
	frac := nsec / 1e9
	if nsec%1e9 >= 1e9/2 {
		frac++
	}
	return ntpTime(sec<<32 | frac)
}
