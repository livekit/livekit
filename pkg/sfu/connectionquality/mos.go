package connectionquality

import (
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/rtcscore-go/pkg/rtcmos"
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

func mosAudioEModel(pctLoss float32, rtt uint32, jitter float32) float32 {
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
	return mosAudioEModel(pctLoss, rtt, jitter)
}

func VideoConnectionScore(pctLoss float32, reducedQuality bool) float32 {
	return loss2Score(pctLoss, reducedQuality)
}
func getBytesFramesFromStreams(streams []*livekit.AnalyticsStream) (totalBytes int64, totalFrames int64) {
	for _, stream := range streams {
		// get frames/bytes/packets from video layers if available
		if len(stream.VideoLayers) > 0 {
			// find max quality 0(LOW), 1(MED), 2(HIGH), 3(OFF)
			videoQuality := -1
			for _, layer := range stream.VideoLayers {
				totalFrames += int64(layer.GetFrames())
				totalBytes += int64(layer.GetBytes())

				// if layer is off or of lower quality than processed, skip
				if (layer.Layer == int32(livekit.VideoQuality_OFF)) || (int32(videoQuality) > layer.Layer) {
					continue
				}
				videoQuality = int(layer.Layer)
			}
		} else {
			totalFrames += int64(stream.Frames)
			totalBytes += int64(stream.GetPrimaryBytes() + stream.GetPaddingBytes() + stream.GetRetransmitBytes())
		}
	}
	return totalBytes, totalFrames
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

func AudioConnectionScoreV2(interval time.Duration, streams []*livekit.AnalyticsStream,
	qualityParam *buffer.ConnectionQualityParams, dtxDisabled bool) float32 {
	totalBytes, _ := getBytesFramesFromStreams(streams)

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

func VideoConnectionScoreV2(interval time.Duration, streams []*livekit.AnalyticsStream, qualityParam *buffer.ConnectionQualityParams,
	codec string) float32 {
	totalBytes, totalFrames := getBytesFramesFromStreams(streams)
	stat := rtcmos.Stat{
		Bitrate:       getBitRate(interval.Seconds(), totalBytes),
		PacketLoss:    qualityParam.LossPercentage,
		RoundTripTime: int32(qualityParam.Rtt),
		BufferDelay:   int32(qualityParam.Jitter),
		VideoConfig: &rtcmos.VideoConfig{
			FrameRate: int32Ptr(getFrameRate(interval.Seconds(), totalFrames)),
			Codec:     codec,
		},
	}
	scores := rtcmos.Score([]rtcmos.Stat{stat})
	if len(scores) == 1 {
		return float32(scores[0].VideoScore)
	}
	return 0
}
