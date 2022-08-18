package connectionquality

import (
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func TestConnectionQuality(t *testing.T) {
	t.Run("audio quality changes over time", func(t *testing.T) {
		cs := NewConnectionStats(ConnectionStatsParams{
			MimeType: "audio/opus",
		})
		cs.Start(&livekit.TrackInfo{Type: livekit.TrackType_AUDIO})

		// first update does nothing
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

		// perfect conditions should return a score of 5
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
		require.Equal(t, float32(5), cs.GetScore())

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

		// layer 0 perfect score
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
		require.Equal(t, float32(5), cs.GetScore())

		maxExpectedLayer = 1
		currentLayer = 1
		// update locks to expected layer and returns previous score
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

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

		// layer 1 at optimal bit rate should give max score
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
		require.Equal(t, float32(5), cs.GetScore())

		// a layer at higher than optimal bit rate should give max score
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
		require.Equal(t, float32(5), cs.GetScore())

		maxExpectedLayer = 2
		currentLayer = 2
		// update locks to expected layer and returns previous score
		cs.updateScore(nil)
		require.Equal(t, float32(5), cs.GetScore())

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

		// layer 2 at optimal bit rate should give max score
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
		require.Equal(t, float32(5), cs.GetScore())

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
		score := cs.GetScore()
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

		// layer 2 max expected, but max available is layer 1 should reduce one level even if layer 1 is perfect
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

		// layer 2 max expected, but max available is layer 0 should reduce two levels even if layer 0 is perfect
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

		// perfect conditions should return a score of 5
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
