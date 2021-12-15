package rtc

import (
	"github.com/livekit/protocol/livekit"
)

// MOS score calculation is based on webrtc-stats
// available @ https://github.com/oanguenot/webrtc-stats

const (
	rtt = 70
)

func score2Rating(score float64) livekit.ConnectionQuality {
	if score > 4.00 {
		return livekit.ConnectionQuality_EXCELLENT
	}

	if score > 2.5 {
		return livekit.ConnectionQuality_GOOD
	}
	return livekit.ConnectionQuality_POOR
}

func mosAudioEmodel(cur, prev *ConnectionStat) float64 {

	if cur == nil {
		return 0.0
	}

	// find percentage of lost packets in this window
	deltaTotalPackets := cur.TotalPackets - prev.TotalPackets
	if deltaTotalPackets == 0 {
		return 0.0
	}

	deltaTotalLostPackets := cur.PacketsLost - prev.PacketsLost
	percentageLost := deltaTotalLostPackets / deltaTotalPackets * 100

	rx := 93.2 - float64(percentageLost)
	ry := 0.18*rx*rx - 27.9*rx + 1126.62

	d := float64(rtt + cur.Jitter)
	h := d - 177.3
	if h < 0 {
		h = 0
	} else {
		h = 1
	}
	id := 0.024*d + 0.11*(d-177.3)*h
	r := ry - (id)
	if r < 0 {
		return 1
	}
	if r > 100 {
		return 4.5
	}
	score := 1 + (0.035 * r) + (7.0/1000000)*r*(r-60)*(100-r)

	return score
}

func ConnectionScore(cur, prev *ConnectionStat, kind livekit.TrackType) float64 {
	if kind == livekit.TrackType_AUDIO {
		return mosAudioEmodel(cur, prev)
	}
	return 0
}
