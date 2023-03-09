package connectionquality

import (
	"math"
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

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

		// introduce loss and the score should drop - 12% loss for Opus -> POOR
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime:   now,
					Duration:    duration,
					Packets:     250,
					PacketsLost: 30,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(3.2), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		// should stay at POOR quality for one iteration even if the conditions improve
		// due to significant loss (12%) in the previous window
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

		// should climb up to GOOD if conditions continue to be good
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

		// should climb up to EXCELLENT if conditions continue to be good
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

		// introduce loss and the score should drop - 5% loss for Opus -> GOOD
		now = now.Add(duration)
		streams = map[uint32]*buffer.StreamStatsWithLayers{
			1: &buffer.StreamStatsWithLayers{
				RTPStats: &buffer.RTPDeltaInfo{
					StartTime:   now,
					Duration:    duration,
					Packets:     250,
					PacketsLost: 13,
				},
			},
		}
		cs.updateScore(streams, now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// should stay at GOOD quality for another iteration even if the conditions improve
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

		// should climb up to EXCELLENT if conditions continue to be good
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

		// a transition to 0 (all layers stopped) should flip quality to EXCELLENT
		now = now.Add(duration)
		cs.AddTransition(0, now)
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)
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
			// NOTE: Because of EWMA (Exponentially Weighted Moving Average), these cut off points are not exact
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
						packetLossPercentage: 5.2,
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
						packetLossPercentage: 4.1,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 8.2,
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
						packetLossPercentage: 22.0,
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
						packetLossPercentage: 2.5,
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
								PacketsLost: uint32(math.Ceil(eq.packetLossPercentage * float64(tc.packetsExpected) / 100.0)),
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
			// NOTE: Because of EWMA (Exponentially Weighted Moving Average), these cut off points are not exact
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
				bytes:           uint64(math.Ceil(7_000_000.0 / 8.0 / 3.5)),
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
				bytes:           uint64(math.Ceil(8_000_000.0 / 8.0 / 13.0)),
				expectedMOS:     3.2,
				expectedQuality: livekit.ConnectionQuality_POOR,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				cs := newConnectionStats("video/vp8", false)

				duration := 5 * time.Second
				now := time.Now()
				cs.Start(&livekit.TrackInfo{Type: livekit.TrackType_VIDEO}, now)

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
}
