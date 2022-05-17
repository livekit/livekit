package connectionquality

import (
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/rtcscore-go/pkg/rtcmos"
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

func getBitRate(interval float64, totalBytes int64) int32 {
	return int32(float64(totalBytes*8) / interval)
}

func getFrameRate(interval float64, totalFrames int64) int32 {
	return int32(float64(totalFrames) / interval)
}

func int32Ptr(x int32) *int32 {
	return &x
}

func AudioConnectionScore(interval time.Duration, totalBytes int64,
	qualityParam *buffer.ConnectionQualityParams, dtxDisabled bool) float32 {

	stat := rtcmos.Stat{
		Bitrate:       getBitRate(interval.Seconds(), totalBytes),
		PacketLoss:    qualityParam.LossPercentage,
		RoundTripTime: int32(qualityParam.Rtt),
		BufferDelay:   int32(qualityParam.Jitter),
		AudioConfig:   &rtcmos.AudioConfig{},
	}

	if dtxDisabled {
		flag := false
		stat.AudioConfig.Dtx = &flag
	}

	scores := rtcmos.Score([]rtcmos.Stat{stat})
	if len(scores) == 1 {
		return float32(scores[0].AudioScore)
	}
	return 0
}

func VideoConnectionScore(interval time.Duration, totalBytes int64, totalFrames int64, qualityParam *buffer.ConnectionQualityParams,
	codec string, expectedHeight int32, expectedWidth int32, actualHeight int32, actualWidth int32) float32 {
	stat := rtcmos.Stat{
		Bitrate:       getBitRate(interval.Seconds(), totalBytes),
		PacketLoss:    qualityParam.LossPercentage,
		RoundTripTime: int32(qualityParam.Rtt),
		BufferDelay:   int32(qualityParam.Jitter),
		VideoConfig: &rtcmos.VideoConfig{
			FrameRate:      int32Ptr(getFrameRate(interval.Seconds(), totalFrames)),
			Codec:          codec,
			ExpectedHeight: &expectedHeight,
			ExpectedWidth:  &expectedWidth,
			Height:         &actualHeight,
			Width:          &actualWidth,
		},
	}
	scores := rtcmos.Score([]rtcmos.Stat{stat})
	if len(scores) == 1 {
		return float32(scores[0].VideoScore)
	}
	return 0
}
