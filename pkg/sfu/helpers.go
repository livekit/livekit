package sfu

import (
	"strings"
	"time"

	"github.com/pion/webrtc/v3"
)

var (
	ntpEpoch = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
)

type ntpTime uint64

const (
	QuarterResolution = "q"
	HalfResolution    = "h"
	FullResolution    = "f"
)

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
