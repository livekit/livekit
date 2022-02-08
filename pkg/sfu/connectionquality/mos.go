package connectionquality

import (
	"github.com/livekit/protocol/livekit"
)

// MOS score calculation is based on webrtc-stats
// available @ https://github.com/oanguenot/webrtc-stats

const (
	defaultRtt = uint32(70)
)

func Score2Rating(score float32) livekit.ConnectionQuality {
	if score > 3.9 {
		return livekit.ConnectionQuality_EXCELLENT
	}

	if score > 2.5 {
		return livekit.ConnectionQuality_GOOD
	}
	return livekit.ConnectionQuality_POOR
}

func mosAudioEmodel(pctLoss float32, rtt uint32, jitter float32) float32 {
	rx := 93.2 - pctLoss
	ry := 0.18*rx*rx - 27.9*rx + 1126.62

	if rtt == 0 {
		rtt = defaultRtt
	}
	// Jitter is in Milliseconds
	d := float32(rtt) + jitter
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

func loss2Score(pctLoss float32, reducedQuality bool) float32 {
	// No Loss, excellent
	if pctLoss == 0.0 && !reducedQuality {
		return 5.0
	}
	// default when loss is minimal, but reducedQuality
	score := float32(3.5)
	// loss is bad
	if pctLoss >= 4.0 {
		score = 2.0
	} else if pctLoss <= 2.0 && !reducedQuality {
		// loss is acceptable and at reduced quality
		score = 4.5
	}
	return score
}

func AudioConnectionScore(pctLoss float32, rtt uint32, jitter float32) float32 {
	return mosAudioEmodel(pctLoss, rtt, jitter)
}

func VideoConnectionScore(pctLoss float32, reducedQuality bool) float32 {
	return loss2Score(pctLoss, reducedQuality)
}
