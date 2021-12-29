package telemetry

import (
	"math"
	"time"

	"github.com/pion/rtcp"
)

const (
	NanoSecondsInMicroSecond        = 1000
	MicroSecondsInSecond     int64  = 1000000
	NtpJan1970Sec            int64  = 2208988800
	MaxCompactNtp            uint32 = 0xFFFFFFFF
	CompactNtpInSecond       int64  = 0x10000
	NtpInSecond              int64  = 1 << 32
)

// SaturatedUsToCompactNtp convert us to rtp compact ntptime field (from webrtc)
func SaturatedUsToCompactNtp(us int64) uint32 {
	if us <= 0 {
		return 0
	}
	if us >= int64(MaxCompactNtp)*MicroSecondsInSecond/CompactNtpInSecond {
		return MaxCompactNtp
	}
	// To convert to compact ntp need to divide by 1e6 to get seconds,
	// then multiply by 0x10000 to get the final result.
	// To avoid float operations, multiplication and division swapped.
	half := (MicroSecondsInSecond - 1) / 2
	quot := us * CompactNtpInSecond / MicroSecondsInSecond
	remain := us * CompactNtpInSecond % MicroSecondsInSecond
	if remain > half {
		return uint32(quot + 1)
	}
	return uint32(quot)
}

// TimeMicrosToNtp convert us to NtpTime (from webrtc)
func TimeMicrosToNtp(time_us int64) *NtpTime {
	timeNtpUs := time_us + NtpJan1970Sec*MicroSecondsInSecond

	// Convert seconds to uint32 through uint64 for well-defined cast.
	// Wrap around (will happen in 2036) is expected for ntp time.
	ntpSeconds := uint32(timeNtpUs / MicroSecondsInSecond)

	// Scale fractions of the second to ntp resolution.
	usFrac := timeNtpUs % MicroSecondsInSecond
	ntpFrac := usFrac * NtpInSecond / MicroSecondsInSecond
	return &NtpTime{ntpSeconds, uint32(ntpFrac)}
}

func NtpToTimeMicros(val NtpTime) int64 {
	sec := int64(val.sec) - NtpJan1970Sec
	micro := int64(val.frac) * MicroSecondsInSecond / NtpInSecond
	return sec*MicroSecondsInSecond + micro
}

func NtpCompactToTime(val uint32) NtpTime {
	sec := (val & 0xFFFF0000) >> 16
	frac := (val & 0x0000FFFF) << 16
	return NtpTime{sec, frac}
}

// NtpTime represents ntp time in rtp
type NtpTime struct {
	sec, frac uint32
}

// Set set ntptime from rtp's ntptime field (uint64)
func (n *NtpTime) Set(val uint64) {
	n.sec = uint32(val >> 32)
	n.frac = uint32(val)
}

// Compact convert ntptime to rtp compact ntptime field (middle 32 bit, uint32)
func (n *NtpTime) Compact() uint32 {
	return (n.sec << 16) | (n.frac >> 16)
}

// FromCompact set ntp fields from compact ntp filed(32bit)
func (n *NtpTime) FromCompact(val uint32) {
	n.sec = (val & 0xFFFF0000) >> 16
	n.frac = (val & 0x0000FFFF) << 16
}

// Ntp return rtp ntptime field
func (n *NtpTime) Ntp() uint64 {
	return (uint64(n.sec) << 32) | (uint64(n.frac))
}

func GetRttMs(packet *rtcp.ReceiverReport) int64 {
	var rttMs int64 = -1
	nowus := time.Now().UnixNano() / NanoSecondsInMicroSecond
	nowntp := TimeMicrosToNtp(nowus).Compact()
	for _, v := range packet.Reports {
		if v.LastSenderReport != 0 {
			currUs := NtpToTimeMicros(NtpCompactToTime(nowntp))
			lastUs := NtpToTimeMicros(NtpCompactToTime(v.LastSenderReport))
			ms := int64(math.Round((float64(currUs-lastUs) / 1000) - (float64(v.Delay) * 1000 / 65536)))
			rttMs = ms
			break
		}
	}
	return rttMs
}
