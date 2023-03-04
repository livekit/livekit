package connectionquality

import (
	"fmt"
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

/* RAJA-REMOVE
func TestConnectionQuality(t *testing.T) {
	t.Run("audio quality changes over time", func(t *testing.T) {
		cs := NewConnectionStats(ConnectionStatsParams{
			MimeType: "audio/opus",
		})
		cs.Start(&livekit.TrackInfo{Type: livekit.TrackType_AUDIO})

		// first update does nothing
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// best conditions should return a high score
		streams := map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    20000 * 2 / 8, // 20 kbps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(4.3), cs.GetScore())

		// introduce loss and the score should drop
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 10,
						Bytes:       18000 * 2 / 8, // 20 kbps - 2 kbps, i. e 10% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		lossQuality := cs.GetScore()
		require.Less(t, lossQuality, float32(3.55))

		// small packet rate, should avoid calculation as it does not meet DTX threshold
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     44,
						PacketsLost: 4,
						Bytes:       1000 * 2 / 8, // 1 kbps, DTX
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, lossQuality, cs.GetScore())

		// setting RTT (i. e. adding delay) should reduce quality
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 10,
						Bytes:       18000 * 2 / 8, // 20 kbps - 2 kbps, i. e 10% lost
						RttMax:      50,            // ms
					},
				},
			},
		}
		cs.updateScore(streams)
		rttQuality := cs.GetScore()
		require.Less(t, rttQuality, lossQuality)

		// adding jitter (i. e. more delay) should reduce quality
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 10,
						Bytes:       18000 * 2 / 8, // 20 kbps - 2 kbps, i. e 10% lost
						RttMax:      50,            // ms
						JitterMax:   10000,         // us
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), rttQuality)
	})

	t.Run("video quality changes over time", func(t *testing.T) {
		maxExpectedLayer := int32(0)
		currentLayer := int32(0)
		isReducedQuality := false

		cs := NewConnectionStats(ConnectionStatsParams{
			MimeType: "video/VP8",
			GetMaxExpectedLayer: func() int32 {
				return maxExpectedLayer
			},
			GetIsReducedQuality: func() (int32, bool) {
				return maxExpectedLayer - currentLayer, isReducedQuality
			},
		})
		cs.Start(&livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Source: livekit.TrackSource_CAMERA,
			Layers: []*livekit.VideoLayer{
				{
					Quality: livekit.VideoQuality_HIGH,
					Width:   1280,
					Height:  720,
					Bitrate: 1_700_000,
				},
				{
					Quality: livekit.VideoQuality_MEDIUM,
					Width:   640,
					Height:  360,
					Bitrate: 500_000,
				},
				{
					Quality: livekit.VideoQuality_LOW,
					Width:   320,
					Height:  180,
					Bitrate: 150_000,
				},
			},
		})

		// first update does nothing
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// second update locks to expected layer
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// layer 0 best conditions
		streams := map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    150_000 * 2 / 8, // 150 kbps
						Frames:   30,              // 15 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		score := cs.GetScore()
		require.Less(t, float32(4.3), score)

		maxExpectedLayer = 1
		currentLayer = 1
		// update locks to expected layer and returns previous score
		cs.updateScore(nil)
		require.Equal(t, score, cs.GetScore())

		// layer 1 at layer 0 bitrate should give lower score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					1: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    150_000 * 2 / 8, // 150 kbps
						Frames:   30,              // 15 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), float32(5))

		// layer 1 at optimal bit rate should give best score for those conditions
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					1: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    500_000 * 2 / 8, // 500 kbps
						Frames:   40,              // 20 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(4.1), cs.GetScore())

		// a layer at higher than optimal bit rate should give high score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					1: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    600_000 * 2 / 8, // 600 kbps, optimal is 500 kbps
						Frames:   40,              // 20 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		score = cs.GetScore()
		require.Less(t, float32(4.2), score)

		maxExpectedLayer = 2
		currentLayer = 2
		// update locks to expected layer and returns previous score
		cs.updateScore(nil)
		require.Equal(t, score, cs.GetScore())

		// layer 2 at layer 1 bitrate should give lower score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    500_000 * 2 / 8, // 500 kbps
						Frames:   40,              // 20 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), float32(5))

		// layer 2 at optimal bit rate should give high score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    1_700_000 * 2 / 8, // 1.7 Mbps
						Frames:   60,                // 30 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(3.75), cs.GetScore())

		// layer 2 at optimal bit rate, but with loss should give less score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    1_530_000 * 2 / 8, // 1.7 Mbps - 170 kbps, i. e. 10% loss
						Frames:   60,                // 30 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		score = cs.GetScore()
		require.Less(t, score, float32(5))

		// adding rtt (i. e. delay) should reduce score further
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    1_530_000 * 2 / 8, // 1.7 Mbps - 170 kbps, i. e. 10% loss
						Frames:   60,                // 30 fps
						RttMax:   50,                // ms
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), score)
		score = cs.GetScore()

		// adding jitter (i. e. delay) should reduce score further
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration:  2 * time.Second,
						Packets:   100,
						Bytes:     1_530_000 * 2 / 8, // 1.7 Mbps - 170 kbps, i. e. 10% loss
						Frames:    60,                // 30 fps
						RttMax:    50,                // ms
						JitterMax: 10000,             // us
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), score)

		// layer 2 max expected, but max available is layer 1 should reduce one level even if layer 1 is at best quality
		currentLayer = 1
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					1: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    500_000 * 2 / 8, // 500 kbps
						Frames:   40,              // 20 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(4), cs.GetScore())

		// layer 2 max expected, but max available is layer 0 should reduce two levels even if layer 0 is at best quality
		currentLayer = 0
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			0: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    150_000 * 2 / 8, // 150 kbps
						Frames:   30,              // 15 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(3), cs.GetScore())
	})

	t.Run("screen share quality changes over time", func(t *testing.T) {
		isReducedQuality := false

		cs := NewConnectionStats(ConnectionStatsParams{
			MimeType: "video/H264",
			GetMaxExpectedLayer: func() int32 {
				return 1
			},
			GetIsReducedQuality: func() (int32, bool) {
				return 0, isReducedQuality
			},
		})
		cs.Start(&livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Source: livekit.TrackSource_SCREEN_SHARE,
			Layers: []*livekit.VideoLayer{
				{
					Width:  1920,
					Height: 1080,
				},
			},
		})

		// first update does nothing
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// second update locks to expected layer
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// best conditions should return a high score
		streams := map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    100000 * 2 / 8, // 100 kbps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(5), cs.GetScore())

		// introduce loss and the score should drop
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 10,
						Bytes:       90000 * 2 / 8, // 100 kbps - 10 kbps, i. e 10% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(2.0), cs.GetScore())

		// small loss, but not reduced quality should give a fairly high score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 2,
						Bytes:       98000 * 2 / 8, // 100 kbps - 2 kbps, i. e. 2% loss
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(4.5), cs.GetScore())

		// medium level loss, but not reduced quality should give a mid level score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 3,
						Bytes:       97000 * 2 / 8, // 100 kbps - 3 kbps, i. e 3% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(3.5), cs.GetScore())

		// medium level loss, reduced quality should give a mid level score
		isReducedQuality = true
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 3,
						Bytes:       97000 * 2 / 8, // 100 kbps - 3 kbps, i. e 3% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(3.5), cs.GetScore())

		// small loss, reduced quality should give a mid level score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 2,
						Bytes:       99000 * 2 / 8, // 100 kbps - 1 kbps, i. e 1% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(3.5), cs.GetScore())
	})
}
*/

func newConnectionStats(mimeType string, isFECEnabled bool) *ConnectionStats {
	return NewConnectionStats(ConnectionStatsParams{
		MimeType:     mimeType,
		IsFECEnabled: isFECEnabled,
		Logger:       logger.GetLogger(),
	})
}

func TestConnectionQuality(t *testing.T) {
	t.Run("quality scorer state machine", func(t *testing.T) {
		cs := newConnectionStats("audio/opus", false)

		duration := 5 * time.Second
		now := time.Now()
		cs.Start(&livekit.TrackInfo{Type: livekit.TrackType_AUDIO}, now.Add(-duration))
		cs.UpdateMute(false, now.Add(-1*time.Second))

		// no data and not enough unmute time should return default state which is EXCELLENT quality
		cs.updateScore(nil, now)
		mos, quality := cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// best conditions (no loss, jitter/rtt = 0) - quality should stay EXCELLENT
		streams := map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime: now,
					Duration:  duration,
					Packets:   250,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// introduce loss and the score should drop - 10% loss for Opus -> POOR
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime:   now,
					Duration:    duration,
					Packets:     250,
					PacketsLost: 25,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(3.2), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		// should stay at POOR quality for poor threshold wait even if the conditions improve
		for i := 0; i < 3; i++ {
			now = now.Add(duration)
			streams = map[uint32]*buffer.StreamStatsWithLayers{
				1: &buffer.StreamStatsWithLayers{
					RTPStats: &buffer.RTPDeltaInfo{
						StartTime: now,
						Duration:  duration,
						Packets:   250,
					},
				},
			}
			cs.updateScore(streams, now.Add(duration))
			mos, quality = cs.GetScoreAndQuality()
			require.Greater(t, float32(3.2), mos)
			require.Equal(t, livekit.ConnectionQuality_POOR, quality)
		}

		// should return median quality which should be EXCELLENT as all windows in above loop have great conditions
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime: now,
					Duration:  duration,
					Packets:   250,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// introduce loss and the score should drop - 3% loss for Opus -> GOOD
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime:   now,
					Duration:    duration,
					Packets:     250,
					PacketsLost: 8,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// should stay at GOOD quality for good threshold (which is shorter than poor threshold) wait even if the conditions improve
		for i := 0; i < 1; i++ {
			now = now.Add(duration)
			streams = map[uint32]*buffer.StreamStatsWithLayers{
				1: &buffer.StreamStatsWithLayers{
					RTPStats: &buffer.RTPDeltaInfo{
						StartTime: now,
						Duration:  duration,
						Packets:   250,
					},
				},
			}
			cs.updateScore(streams, now.Add(duration))
			mos, quality = cs.GetScoreAndQuality()
			require.Greater(t, float32(4.1), mos)
			require.Equal(t, livekit.ConnectionQuality_GOOD, quality)
		}

		// should return median quality which should be EXCELLENT as all windows in above loop have great conditions
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime: now,
					Duration:  duration,
					Packets:   250,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// POOR -> GOOD -> EXCELLENT should take longer
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime:   now,
					Duration:    duration,
					Packets:     250,
					PacketsLost: 25,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(3.2), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		// should stay at POOR quality for poor threshold wait even if the conditions improve
		for i := 0; i < 3; i++ {
			now = now.Add(duration)
			streams = map[uint32]*buffer.StreamStatsWithLayers{
				1: &buffer.StreamStatsWithLayers{
					RTPStats: &buffer.RTPDeltaInfo{
						StartTime:   now,
						Duration:    duration,
						Packets:     250,
						PacketsLost: 8,
					},
				},
			}
			cs.updateScore(streams, now.Add(duration))
			mos, quality = cs.GetScoreAndQuality()
			require.Greater(t, float32(3.2), mos)
			require.Equal(t, livekit.ConnectionQuality_POOR, quality)
		}

		// should return median quality which should be GOOD as all windows in above loop have some loss (i. e. GOOD quality).
		// although the below update has no loss (EXCELLENT quality), median should be at GOOO
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime: now,
					Duration:  duration,
					Packets:   250,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// another EXCELLENT quality window and the median should switch to EXCELLENT
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime: now,
					Duration:  duration,
					Packets:   250,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// mute when quality is POOR should return quality to EXCELLENT
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime:   now,
					Duration:    duration,
					Packets:     250,
					PacketsLost: 25,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(3.2), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		now = now.Add(duration)
		cs.UpdateMute(true, now.Add(1*time.Second))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// unmute at time so that next window does not satisfy the unmute time threshold.
		// that means even if the next update has 0 packets, it should hold state and stay at EXCELLENT quality
		cs.UpdateMute(false, now.Add(3*time.Second))

		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime: now,
					Duration:  duration,
					Packets:   0,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		fmt.Printf("last update\n") // REMOVE
		// next update with no packets should knock quality down
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime: now,
					Duration:  duration,
					Packets:   0,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(3.2), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		// mute/unmute to bring quality back up
		now = now.Add(duration)
		cs.UpdateMute(true, now.Add(1*time.Second))
		cs.UpdateMute(false, now.Add(2*time.Second))

		// with lesser number of packet (simulating DTX).
		// even higher loss (like 10%) should only knock down quality to GOOD, typically would be POOR at that loss rate
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime:   now,
					Duration:    duration,
					Packets:     50,
					PacketsLost: 5,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// mute/unmute to bring quality back up
		now = now.Add(duration)
		cs.UpdateMute(true, now.Add(1*time.Second))
		cs.UpdateMute(false, now.Add(2*time.Second))

		// RTT and jitter can knock quality down.
		// at 2% loss, quality should stay at EXCELLENT purely based on loss, but with added RTT/jitter, should drop to GOOD
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime:   now,
					Duration:    duration,
					Packets:     250,
					PacketsLost: 5,
					RttMax:      300,
					JitterMax:   30000,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// mute/unmute to bring quality back up
		now = now.Add(duration)
		cs.UpdateMute(true, now.Add(1*time.Second))
		cs.UpdateMute(false, now.Add(2*time.Second))

		// bitrate based calculation can drop quality even if there is no loss
		cs.AddTransition(1_000_000, now)
		cs.AddTransition(2_000_000, now.Add(2*time.Second))

		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime: now,
					Duration:  duration,
					Packets:   250,
					Bytes:     8_000_000 / 8 / 4,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)
	})

	t.Run("codecs - packets", func(t *testing.T) {
		type expectedQuality struct {
			packetLossPercentage float64
			expectedMOS          float32
			expectedQuality      livekit.ConnectionQuality
		}
		testCases := []struct {
			name              string
			mimeType          string
			isFECEnabled      bool
			packetsExpected   uint32
			expectedQualities []expectedQuality
		}{
			// "audio/opus" - no fec - 0 <= loss < 2.5%: EXCELLENT, 2.5% <= loss < 5%: GOOD, >= 5%: POOR
			{
				name:            "audio/opus - no fec",
				mimeType:        "audio/opus",
				isFECEnabled:    false,
				packetsExpected: 200,
				expectedQualities: []expectedQuality{
					{
						packetLossPercentage: 1.0,
						expectedMOS:          4.6,
						expectedQuality:      livekit.ConnectionQuality_EXCELLENT,
					},
					{
						packetLossPercentage: 4.0,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 5.0,
						expectedMOS:          3.2,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
			// "audio/opus" - fec - 0 <= loss < 3.75%: EXCELLENT, 3.75% <= loss < 7.5%: GOOD, >= 7.5%: POOR
			{
				name:            "audio/opus - fec",
				mimeType:        "audio/opus",
				isFECEnabled:    true,
				packetsExpected: 200,
				expectedQualities: []expectedQuality{
					{
						packetLossPercentage: 3.0,
						expectedMOS:          4.6,
						expectedQuality:      livekit.ConnectionQuality_EXCELLENT,
					},
					{
						packetLossPercentage: 4.0,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 8.0,
						expectedMOS:          3.2,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
			// "audio/red" - no fec - 0 <= loss < 6.66%: EXCELLENT, 6.66% <= loss < 13.33%: GOOD, >= 13.33%: POOR
			{
				name:            "audio/red - no fec",
				mimeType:        "audio/red",
				isFECEnabled:    false,
				packetsExpected: 200,
				expectedQualities: []expectedQuality{
					{
						packetLossPercentage: 6.0,
						expectedMOS:          4.6,
						expectedQuality:      livekit.ConnectionQuality_EXCELLENT,
					},
					{
						packetLossPercentage: 10.0,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 16.0,
						expectedMOS:          3.2,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
			// "audio/red" - fec - 0 <= loss < 10%: EXCELLENT, 10% <= loss < 20%: GOOD, >= 20%: POOR
			{
				name:            "audio/red - fec",
				mimeType:        "audio/red",
				isFECEnabled:    true,
				packetsExpected: 200,
				expectedQualities: []expectedQuality{
					{
						packetLossPercentage: 8.0,
						expectedMOS:          4.6,
						expectedQuality:      livekit.ConnectionQuality_EXCELLENT,
					},
					{
						packetLossPercentage: 18.0,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 20.0,
						expectedMOS:          3.2,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
			// "video/*" - 0 <= loss < 2%: EXCELLENT, 2% <= loss < 4%: GOOD, >= 4%: POOR
			{
				name:            "video/*",
				mimeType:        "video/vp8",
				isFECEnabled:    false,
				packetsExpected: 200,
				expectedQualities: []expectedQuality{
					{
						packetLossPercentage: 1.0,
						expectedMOS:          4.6,
						expectedQuality:      livekit.ConnectionQuality_EXCELLENT,
					},
					{
						packetLossPercentage: 2.0,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 5.0,
						expectedMOS:          3.2,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				cs := newConnectionStats(tc.mimeType, tc.isFECEnabled)

				duration := 5 * time.Second
				now := time.Now()
				cs.Start(&livekit.TrackInfo{Type: livekit.TrackType_AUDIO}, now.Add(-duration))

				for _, eq := range tc.expectedQualities {
					streams := map[uint32]*buffer.StreamStatsWithLayers{
						123: &buffer.StreamStatsWithLayers{
							RTPStats: &buffer.RTPDeltaInfo{
								StartTime:   now,
								Duration:    duration,
								Packets:     tc.packetsExpected,
								PacketsLost: uint32(eq.packetLossPercentage * float64(tc.packetsExpected) / 100.0),
							},
						},
					}
					cs.updateScore(streams, now.Add(duration))
					mos, quality := cs.GetScoreAndQuality()
					require.Greater(t, eq.expectedMOS, mos)
					require.Equal(t, eq.expectedQuality, quality)

					now = now.Add(duration)
				}
			})
		}
	})

	t.Run("bytes", func(t *testing.T) {
		type transition struct {
			bitrate int64
			offset  time.Duration
		}
		testCases := []struct {
			name            string
			transitions     []transition
			bytes           uint64
			expectedMOS     float32
			expectedQuality livekit.ConnectionQuality
		}{
			// 1.0 <= expectedBits / actualBits < ~2.7 = EXCELLENT
			// ~2.7 <= expectedBits / actualBits < ~7.5 = GOOD
			// expectedBits / actualBits >= ~7.5 = POOR
			{
				name: "excellent",
				transitions: []transition{
					{
						bitrate: 1_000_000,
					},
					{
						bitrate: 2_000_000,
						offset:  3 * time.Second,
					},
				},
				bytes:           6_000_000 / 8,
				expectedMOS:     4.6,
				expectedQuality: livekit.ConnectionQuality_EXCELLENT,
			},
			{
				name: "good",
				transitions: []transition{
					{
						bitrate: 1_000_000,
					},
					{
						bitrate: 2_000_000,
						offset:  3 * time.Second,
					},
				},
				bytes:           7_000_000 / 8 / 3,
				expectedMOS:     4.1,
				expectedQuality: livekit.ConnectionQuality_GOOD,
			},
			{
				name: "poor",
				transitions: []transition{
					{
						bitrate: 2_000_000,
					},
					{
						bitrate: 1_000_000,
						offset:  3 * time.Second,
					},
				},
				bytes:           8_000_000 / 8 / 8,
				expectedMOS:     3.2,
				expectedQuality: livekit.ConnectionQuality_POOR,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				cs := newConnectionStats("video/vp8", false)

				duration := 5 * time.Second
				now := time.Now()
				cs.Start(&livekit.TrackInfo{Type: livekit.TrackType_AUDIO}, now.Add(-duration))

				for _, tr := range tc.transitions {
					cs.AddTransition(tr.bitrate, now.Add(tr.offset))
				}

				streams := map[uint32]*buffer.StreamStatsWithLayers{
					123: &buffer.StreamStatsWithLayers{
						RTPStats: &buffer.RTPDeltaInfo{
							StartTime: now,
							Duration:  duration,
							Packets:   100,
							Bytes:     tc.bytes,
						},
					},
				}
				cs.updateScore(streams, now.Add(duration))
				mos, quality := cs.GetScoreAndQuality()
				require.Greater(t, tc.expectedMOS, mos)
				require.Equal(t, tc.expectedQuality, quality)
			})
		}
	})

	/* RAJA-TODO
	t.Run("video quality changes over time", func(t *testing.T) {
		maxExpectedLayer := int32(0)
		currentLayer := int32(0)
		isReducedQuality := false

		cs := NewConnectionStats(ConnectionStatsParams{
			MimeType: "video/VP8",
			GetMaxExpectedLayer: func() int32 {
				return maxExpectedLayer
			},
			GetIsReducedQuality: func() (int32, bool) {
				return maxExpectedLayer - currentLayer, isReducedQuality
			},
		})
		cs.Start(&livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Source: livekit.TrackSource_CAMERA,
			Layers: []*livekit.VideoLayer{
				{
					Quality: livekit.VideoQuality_HIGH,
					Width:   1280,
					Height:  720,
					Bitrate: 1_700_000,
				},
				{
					Quality: livekit.VideoQuality_MEDIUM,
					Width:   640,
					Height:  360,
					Bitrate: 500_000,
				},
				{
					Quality: livekit.VideoQuality_LOW,
					Width:   320,
					Height:  180,
					Bitrate: 150_000,
				},
			},
		})

		// first update does nothing
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// second update locks to expected layer
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// layer 0 best conditions
		streams := map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    150_000 * 2 / 8, // 150 kbps
						Frames:   30,              // 15 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		score := cs.GetScore()
		require.Less(t, float32(4.3), score)

		maxExpectedLayer = 1
		currentLayer = 1
		// update locks to expected layer and returns previous score
		cs.updateScore(nil)
		require.Equal(t, score, cs.GetScore())

		// layer 1 at layer 0 bitrate should give lower score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					1: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    150_000 * 2 / 8, // 150 kbps
						Frames:   30,              // 15 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), float32(5))

		// layer 1 at optimal bit rate should give best score for those conditions
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					1: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    500_000 * 2 / 8, // 500 kbps
						Frames:   40,              // 20 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(4.1), cs.GetScore())

		// a layer at higher than optimal bit rate should give high score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					1: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    600_000 * 2 / 8, // 600 kbps, optimal is 500 kbps
						Frames:   40,              // 20 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		score = cs.GetScore()
		require.Less(t, float32(4.2), score)

		maxExpectedLayer = 2
		currentLayer = 2
		// update locks to expected layer and returns previous score
		cs.updateScore(nil)
		require.Equal(t, score, cs.GetScore())

		// layer 2 at layer 1 bitrate should give lower score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    500_000 * 2 / 8, // 500 kbps
						Frames:   40,              // 20 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), float32(5))

		// layer 2 at optimal bit rate should give high score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    1_700_000 * 2 / 8, // 1.7 Mbps
						Frames:   60,                // 30 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(3.75), cs.GetScore())

		// layer 2 at optimal bit rate, but with loss should give less score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    1_530_000 * 2 / 8, // 1.7 Mbps - 170 kbps, i. e. 10% loss
						Frames:   60,                // 30 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		score = cs.GetScore()
		require.Less(t, score, float32(5))

		// adding rtt (i. e. delay) should reduce score further
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    1_530_000 * 2 / 8, // 1.7 Mbps - 170 kbps, i. e. 10% loss
						Frames:   60,                // 30 fps
						RttMax:   50,                // ms
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), score)
		score = cs.GetScore()

		// adding jitter (i. e. delay) should reduce score further
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					2: &buffer.RTPDeltaInfo{
						Duration:  2 * time.Second,
						Packets:   100,
						Bytes:     1_530_000 * 2 / 8, // 1.7 Mbps - 170 kbps, i. e. 10% loss
						Frames:    60,                // 30 fps
						RttMax:    50,                // ms
						JitterMax: 10000,             // us
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, cs.GetScore(), score)

		// layer 2 max expected, but max available is layer 1 should reduce one level even if layer 1 is at best quality
		currentLayer = 1
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					1: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    500_000 * 2 / 8, // 500 kbps
						Frames:   40,              // 20 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(4), cs.GetScore())

		// layer 2 max expected, but max available is layer 0 should reduce two levels even if layer 0 is at best quality
		currentLayer = 0
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			0: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    150_000 * 2 / 8, // 150 kbps
						Frames:   30,              // 15 fps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Less(t, float32(3), cs.GetScore())
	})
	*/

	/* RAJA-TODO
	t.Run("screen share quality changes over time", func(t *testing.T) {
		isReducedQuality := false

		cs := NewConnectionStats(ConnectionStatsParams{
			MimeType: "video/H264",
			GetMaxExpectedLayer: func() int32 {
				return 1
			},
			GetIsReducedQuality: func() (int32, bool) {
				return 0, isReducedQuality
			},
		})
		cs.Start(&livekit.TrackInfo{
			Type:   livekit.TrackType_VIDEO,
			Source: livekit.TrackSource_SCREEN_SHARE,
			Layers: []*livekit.VideoLayer{
				{
					Width:  1920,
					Height: 1080,
				},
			},
		})

		// first update does nothing
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// second update locks to expected layer
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// best conditions should return a high score
		streams := map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration: 2 * time.Second,
						Packets:  100,
						Bytes:    100000 * 2 / 8, // 100 kbps
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(5), cs.GetScore())

		// introduce loss and the score should drop
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 10,
						Bytes:       90000 * 2 / 8, // 100 kbps - 10 kbps, i. e 10% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(2.0), cs.GetScore())

		// small loss, but not reduced quality should give a fairly high score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 2,
						Bytes:       98000 * 2 / 8, // 100 kbps - 2 kbps, i. e. 2% loss
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(4.5), cs.GetScore())

		// medium level loss, but not reduced quality should give a mid level score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 3,
						Bytes:       97000 * 2 / 8, // 100 kbps - 3 kbps, i. e 3% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(3.5), cs.GetScore())

		// medium level loss, reduced quality should give a mid level score
		isReducedQuality = true
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 3,
						Bytes:       97000 * 2 / 8, // 100 kbps - 3 kbps, i. e 3% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(3.5), cs.GetScore())

		// small loss, reduced quality should give a mid level score
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				Layers: map[int32]*buffer.RTPDeltaInfo{
					0: &buffer.RTPDeltaInfo{
						Duration:    2 * time.Second,
						Packets:     100,
						PacketsLost: 2,
						Bytes:       99000 * 2 / 8, // 100 kbps - 1 kbps, i. e 1% lost
					},
				},
			},
		}
		cs.updateScore(streams)
		require.Equal(t, float32(3.5), cs.GetScore())
	})
	*/
}
