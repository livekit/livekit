package sfu

import (
	"math"
	"strings"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
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

// -----------------------------------------------

func getRttMs(report *rtcp.ReceptionReport) uint32 {
	if report.LastSenderReport == 0 {
		return 0
	}

	// RTT calculation reference: https://datatracker.ietf.org/doc/html/rfc3550#section-6.4.1

	// middle 32-bits of current NTP time
	now := uint32(buffer.ToNtpTime(time.Now()) >> 16)
	if now < (report.LastSenderReport + report.Delay) {
		return 0
	}
	ntpDiff := now - report.LastSenderReport - report.Delay
	return uint32(math.Ceil(float64(ntpDiff) * 1000.0 / 65536.0))
}
