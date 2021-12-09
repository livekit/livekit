package rtc

import (
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// MOS score calculation is based on webrtc-stats
// available @ https://github.com/oanguenot/webrtc-stats

const (
	MosExcellent = "Excellent"
	MosGood      = "Good"
	MosFair      = "Fair"
	MosPoor      = "Poor"
	MosBad       = "Bad"
	rtt          = 10
)

func score2Rating(score float64) string {
	if score > 4.30 {
		return MosExcellent
	}
	if score > 4.00 {
		return MosGood
	}
	if score > 3.6 {
		return MosFair
	}
	if score > 3.1 {
		return MosPoor
	}
	return MosBad
}

func mosAudioEmodel(cur *MediaStats) float64 {

	if cur == nil {
		return 0.0
	}

	percentageLost := cur.FractionLost * 100
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

func MosRating(cur *MediaStats, kind livekit.TrackType) (float64, string) {
	if kind == livekit.TrackType_AUDIO {
		score := mosAudioEmodel(cur)
		rating := score2Rating(score)
		logger.Debugw("---------", "score", score, "rating", rating)
		return score, rating
	}
	return 0, ""
}
