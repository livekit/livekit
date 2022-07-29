package connectionquality

import (
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/rtcscore-go/pkg/rtcmos"
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

func getBitRate(interval float64, bytes uint64) int32 {
	return int32(float64(bytes*8) / interval)
}

func getFrameRate(interval float64, frames uint32) int32 {
	return int32(float64(frames) / interval)
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

type TrackScoreParams struct {
	Duration         time.Duration
	Codec            string
	PacketsExpected  uint32
	PacketsLost      uint32
	Bytes            uint64
	Frames           uint32
	Jitter           float64
	Rtt              uint32
	DtxDisabled      bool
	ActualWidth      uint32
	ActualHeight     uint32
	ExpectedWidth    uint32
	ExpectedHeight   uint32
	IsReducedQuality bool
}

func getRtcMosStat(params TrackScoreParams) rtcmos.Stat {
	return rtcmos.Stat{
		Bitrate:       getBitRate(params.Duration.Seconds(), params.Bytes),
		PacketLoss:    getLossPercentage(params.PacketsExpected, params.PacketsLost),
		RoundTripTime: int32Ptr(int32(params.Rtt)),
		BufferDelay:   int32Ptr(int32(params.Jitter / 1000.0)),
	}
}

func AudioTrackScore(params TrackScoreParams) float32 {
	stat := getRtcMosStat(params)
	stat.AudioConfig = &rtcmos.AudioConfig{}
	if !params.DtxDisabled {
		flag := true
		stat.AudioConfig.Dtx = &flag
	}

	scores := rtcmos.Score([]rtcmos.Stat{stat})
	if len(scores) == 1 {
		return float32(scores[0].AudioScore)
	}
	return 0
}

func VideoTrackScore(params TrackScoreParams) float32 {
	stat := getRtcMosStat(params)
	stat.VideoConfig = &rtcmos.VideoConfig{
		FrameRate:      int32Ptr(getFrameRate(params.Duration.Seconds(), params.Frames)),
		Codec:          params.Codec,
		ExpectedWidth:  int32Ptr(int32(params.ExpectedWidth)),
		ExpectedHeight: int32Ptr(int32(params.ExpectedHeight)),
		Width:          int32Ptr(int32(params.ActualWidth)),
		Height:         int32Ptr(int32(params.ActualHeight)),
	}

	scores := rtcmos.Score([]rtcmos.Stat{stat})
	if len(scores) == 1 {
		return float32(scores[0].VideoScore)
	}
	return 0
}

//
// rtcmos gives lower score when screen share content is static.
// Even though the frame rate is low, the bit rate is also low and
// the resolution is high. Till rtcmos model can be adapted to that
// scenario, use loss based scoring.
//
func ScreenshareTrackScore(params TrackScoreParams) float32 {
	pctLoss := getLossPercentage(params.PacketsExpected, params.PacketsLost)
	// No Loss, excellent
	if pctLoss == 0.0 && !params.IsReducedQuality {
		return 5.0
	}
	// default when loss is minimal, but reducedQuality
	score := float32(3.5)
	// loss is bad
	if pctLoss >= 4.0 {
		score = 2.0
	} else if pctLoss <= 2.0 && !params.IsReducedQuality {
		// loss is acceptable and at reduced quality
		score = 4.5
	}
	return score
}
