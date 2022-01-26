package connectionquality

import (
	"github.com/livekit/protocol/livekit"
)

// MOS score calculation is based on webrtc-stats
// available @ https://github.com/oanguenot/webrtc-stats

const (
	rtt = 70
)

func Score2Rating(score float64) livekit.ConnectionQuality {
	if score > 3.9 {
		return livekit.ConnectionQuality_EXCELLENT
	}

	if score > 2.5 {
		return livekit.ConnectionQuality_GOOD
	}
	return livekit.ConnectionQuality_POOR
}

func mosAudioEmodel(delta DeltaStats, jitter uint32) float64 {

	// find percentage of lost packets in this window
	if delta.TotalPackets == 0 {
		return 0.0
	}

	percentageLost := (float64(delta.PacketsLost) / float64(delta.TotalPackets)) * 100

	rx := 93.2 - percentageLost
	ry := 0.18*rx*rx - 27.9*rx + 1126.62

	// Jitter is in MicroSecs (1/1e6) units. Convert it to MilliSecs
	d := float64(rtt + (jitter / 1000))
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

func loss2Score(loss uint32, reducedQuality bool) float64 {
	// No Loss, excellent
	if loss == 0 && !reducedQuality {
		return 5
	}
	// default when loss is minimal, but reducedQuality
	score := 3.5
	// loss is bad
	if loss >= 4 {
		score = 2.0
	} else if loss <= 2 && !reducedQuality {
		// loss is acceptable and at reduced quality
		score = 4.5
	}
	return score
}

func AudioConnectionScore(delta DeltaStats, jitter uint32) float64 {
	return mosAudioEmodel(delta, jitter)
}

func VideoConnectionScore(pctLoss uint32, reducedQuality bool) float64 {
	return loss2Score(pctLoss, reducedQuality)
}
