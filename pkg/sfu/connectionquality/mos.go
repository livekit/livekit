package connectionquality

import (
	"math"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/rtcscore-go/pkg/rtcmos"
)

const (
	MinScore = float32(1)
	MaxScore = float32(5)
)

func Score2Rating(score float32) livekit.ConnectionQuality {
	if score > 4.0 {
		return livekit.ConnectionQuality_EXCELLENT
	}

	if score > 3.0 {
		return livekit.ConnectionQuality_GOOD
	}
	return livekit.ConnectionQuality_POOR
}

func clamp(value, min, max float64) float64 {
	return math.Max(min, math.Min(value, max))
}

func getBitRate(interval float64, bytes uint64) float64 {
	return float64(bytes*8) / interval
}

func getFrameRate(interval float64, frames uint32) float64 {
	return float64(frames) / interval
}

func getLossPercentage(expected uint32, lost uint32) float32 {
	if expected == 0 {
		return 0.0
	}

	return float32(lost) * 100.0 / float32(expected)
}

func int32Ptr(x int32) *int32 {
	return &x
}

func float32Ptr(x float32) *float32 {
	return &x
}

type TrackScoreParams struct {
	Duration          time.Duration
	Codec             string
	PacketsExpected   uint32
	PacketsLost       uint32
	Bytes             uint64
	Frames            uint32
	FrameRateExpected uint32
	Jitter            float64
	Rtt               uint32
	DtxEnabled        bool
	Width             uint32
	Height            uint32
	IsReducedQuality  bool
}

func getRtcMosStat(params TrackScoreParams) rtcmos.Stat {
	return rtcmos.Stat{
		Bitrate:       float32(getBitRate(params.Duration.Seconds(), params.Bytes)),
		PacketLoss:    getLossPercentage(params.PacketsExpected, params.PacketsLost),
		RoundTripTime: int32Ptr(int32(params.Rtt)),
		BufferDelay:   int32Ptr(int32(params.Jitter / 1000.0)),
	}
}

func AudioTrackScore(params TrackScoreParams, normFactor float32) float32 {
	stat := getRtcMosStat(params)
	stat.AudioConfig = &rtcmos.AudioConfig{}
	stat.AudioConfig.Dtx = &params.DtxEnabled

	scores := rtcmos.Score([]rtcmos.Stat{stat})
	if len(scores) == 1 {
		return float32(clamp(float64(float32(scores[0].AudioScore)*normFactor), float64(MinScore), float64(MaxScore)))
	}
	return 0
}

func VideoTrackScore(params TrackScoreParams, normFactor float32) float32 {
	stat := getRtcMosStat(params)
	stat.VideoConfig = &rtcmos.VideoConfig{
		FrameRate: float32Ptr(float32(getFrameRate(params.Duration.Seconds(), params.Frames))),
		Codec:     params.Codec,
		Width:     int32Ptr(int32(params.Width)),
		Height:    int32Ptr(int32(params.Height)),
	}
	if params.FrameRateExpected == 0 {
		stat.VideoConfig.ExpectedFrameRate = stat.VideoConfig.FrameRate
	}

	scores := rtcmos.Score([]rtcmos.Stat{stat})
	if len(scores) == 1 {
		return float32(clamp(float64(float32(scores[0].VideoScore)*normFactor), float64(MinScore), float64(MaxScore)))
	}
	return 0
}

//
// rtcmos gives lower score when screen share content is static.
// That is due to use of bits / pixel / frame in the model.
// Even though the frame rate is low, the bit rate is also low and
// the resolution is high. Till rtcmos model can be adapted to that
// scenario, use loss based scoring.
//
func LossBasedTrackScore(params TrackScoreParams) float32 {
	pctLoss := getLossPercentage(params.PacketsExpected, params.PacketsLost)
	// No Loss, excellent
	if pctLoss == 0.0 && !params.IsReducedQuality {
		return MaxScore
	}
	// default when loss is minimal, but reducedQuality
	score := float32(3.5)
	// loss is bad
	if pctLoss >= 4.0 {
		score = 2.0
	} else if pctLoss <= 2.0 && !params.IsReducedQuality {
		// loss is acceptable and not at reduced quality
		score = 4.5
	}
	return score
}
