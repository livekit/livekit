// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connectionquality

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// -----------------------------------------------

type testReceiverProvider struct {
	streams              map[uint32]*buffer.StreamStatsWithLayers
	lastSenderReportTime time.Time
}

func newTestReceiverProvider() *testReceiverProvider {
	return &testReceiverProvider{}
}

func (trp *testReceiverProvider) setStreams(streams map[uint32]*buffer.StreamStatsWithLayers) {
	trp.streams = streams
}

func (trp *testReceiverProvider) GetDeltaStats() map[uint32]*buffer.StreamStatsWithLayers {
	return trp.streams
}

func (trp *testReceiverProvider) setLastSenderReportTime(at time.Time) {
	trp.lastSenderReportTime = at
}

func (trp *testReceiverProvider) GetLastSenderReportTime() time.Time {
	return trp.lastSenderReportTime
}

// -----------------------------------------------

func TestConnectionQuality(t *testing.T) {
	trp := newTestReceiverProvider()
	t.Run("quality scorer operation", func(t *testing.T) {
		cs := NewConnectionStats(ConnectionStatsParams{
			IncludeRTT:         true,
			IncludeJitter:      true,
			EnableBitrateScore: true,
			ReceiverProvider:   trp,
			Logger:             logger.GetLogger(),
		})

		duration := 5 * time.Second
		now := time.Now()
		cs.StartAt(mime.MimeTypeOpus, false, now.Add(-duration))
		cs.UpdateMuteAt(false, now.Add(-1*time.Second))

		// no data and not enough unmute time should return default state which is EXCELLENT quality
		cs.updateScoreAt(now)
		mos, quality := cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// best conditions (no loss, jitter/rtt = 0) - quality should stay EXCELLENT
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// introduce loss and the score should drop - 12% loss for Opus -> POOR
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime:   now,
					EndTime:     now.Add(duration),
					Packets:     120,
					PacketsLost: 30,
				},
			},
			2: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime:   now,
					EndTime:     now.Add(duration),
					Packets:     130,
					PacketsLost: 0,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(2.1), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		// should climb to GOOD quality in one iteration if the conditions improve.
		// although significant loss (12%) in the previous window, lowest score is
		// bound so that climbing back does not take too long even under excellent conditions.
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// should stay at GOOD if conditions continue to be good
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// should climb up to EXCELLENT if conditions continue to be good
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// introduce loss and the score should drop - 5% loss for Opus -> GOOD
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime:   now,
					EndTime:     now.Add(duration),
					Packets:     250,
					PacketsLost: 13,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// should stay at GOOD quality for another iteration even if the conditions improve
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// should climb up to EXCELLENT if conditions continue to be good
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// mute when quality is POOR should return quality to EXCELLENT
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime:   now,
					EndTime:     now.Add(duration),
					Packets:     250,
					PacketsLost: 30,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(2.1), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		now = now.Add(duration)
		cs.UpdateMuteAt(true, now.Add(1*time.Second))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// unmute at specific time to ensure next window does not satisfy the unmute time threshold.
		// that means even if the next update has 0 packets, it should hold state and stay at EXCELLENT quality
		cs.UpdateMuteAt(false, now.Add(3*time.Second))

		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   0,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// next update with no packets,
		// but last RTCP is not set, should knock quality down to POOR
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   0,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(2.1), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		// another dry spell, but last RTCP is not stale, should keep quality at POOR
		now = now.Add(duration)
		trp.setLastSenderReportTime(now.Add(time.Second))
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   0,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(2.1), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		// yet another dry spell, but last RTCP is stale, should knock down quality at LOST
		now = now.Add(duration)
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   0,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(1.3), mos)
		require.Equal(t, livekit.ConnectionQuality_LOST, quality)

		// mute when LOST should not bump up score/quality
		now = now.Add(duration)
		cs.UpdateMuteAt(true, now.Add(1*time.Second))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(1.3), mos)
		require.Equal(t, livekit.ConnectionQuality_LOST, quality)

		// unmute and send packets to bring quality back up
		now = now.Add(duration)
		cs.UpdateMuteAt(false, now.Add(2*time.Second))
		for i := 0; i < 3; i++ {
			trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
				1: {
					RTPStats: &rtpstats.RTPDeltaInfo{
						StartTime:   now,
						EndTime:     now.Add(duration),
						Packets:     250,
						PacketsLost: 0,
					},
				},
			})
			cs.updateScoreAt(now.Add(duration))
			now = now.Add(duration)
		}
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// with lesser number of packet (simulating DTX).
		// even higher loss (like 10%) should not knock down quality due to quadratic weighting of packet loss ratio
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime:   now,
					EndTime:     now.Add(duration),
					Packets:     50,
					PacketsLost: 5,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// mute/unmute to bring quality back up
		now = now.Add(duration)
		cs.UpdateMuteAt(true, now.Add(1*time.Second))
		cs.UpdateMuteAt(false, now.Add(2*time.Second))

		// RTT and jitter can knock quality down.
		// at 2% loss, quality should stay at EXCELLENT purely based on loss, but with added RTT/jitter, should drop to GOOD
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime:   now,
					EndTime:     now.Add(duration),
					Packets:     250,
					PacketsLost: 5,
					RttMax:      400,
					JitterMax:   30000,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// mute/unmute to bring quality back up
		now = now.Add(duration)
		cs.UpdateMuteAt(true, now.Add(1*time.Second))
		cs.UpdateMuteAt(false, now.Add(2*time.Second))

		// bitrate based calculation can drop quality even if there is no loss
		cs.AddBitrateTransitionAt(1_000_000, now)
		cs.AddBitrateTransitionAt(2_000_000, now.Add(2*time.Second))

		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
					Bytes:     8_000_000 / 8 / 5,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		// test layer mute via UpdateLayerMute API
		cs.AddBitrateTransitionAt(1_000_000, now)
		cs.AddBitrateTransitionAt(2_000_000, now.Add(2*time.Second))

		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
					Bytes:     8_000_000 / 8 / 5,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)

		now = now.Add(duration)
		cs.UpdateLayerMuteAt(true, now)
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// unmute layer
		cs.UpdateLayerMuteAt(false, now.Add(2*time.Second))

		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
					Bytes:     8_000_000 / 8 / 5,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)

		// pause
		now = now.Add(duration)
		cs.UpdatePauseAt(true, now)
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(2.1), mos)
		require.Equal(t, livekit.ConnectionQuality_POOR, quality)

		// resume
		cs.UpdatePauseAt(false, now.Add(2*time.Second))

		// although conditions are perfect, climbing back from POOR (because of pause above)
		// will only climb to GOOD.
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime: now,
					EndTime:   now.Add(duration),
					Packets:   250,
					Bytes:     8_000_000 / 8 / 5,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality = cs.GetScoreAndQuality()
		require.Greater(t, float32(4.1), mos)
		require.Equal(t, livekit.ConnectionQuality_GOOD, quality)
	})

	t.Run("quality scorer dependent rtt", func(t *testing.T) {
		cs := NewConnectionStats(ConnectionStatsParams{
			IncludeRTT:       false,
			IncludeJitter:    true,
			ReceiverProvider: trp,
			Logger:           logger.GetLogger(),
		})

		duration := 5 * time.Second
		now := time.Now()
		cs.StartAt(mime.MimeTypeOpus, false, now.Add(-duration))
		cs.UpdateMuteAt(false, now.Add(-1*time.Second))

		// RTT does not knock quality down because it is dependent and hence not taken into account
		// at 2% loss, quality should stay at EXCELLENT purely based on loss. With high RTT (700 ms)
		// quality should drop to GOOD if RTT were taken into consideration
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime:   now,
					EndTime:     now.Add(duration),
					Packets:     250,
					PacketsLost: 5,
					RttMax:      700,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality := cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)
	})

	t.Run("quality scorer dependent jitter", func(t *testing.T) {
		cs := NewConnectionStats(ConnectionStatsParams{
			IncludeRTT:       true,
			IncludeJitter:    false,
			ReceiverProvider: trp,
			Logger:           logger.GetLogger(),
		})

		duration := 5 * time.Second
		now := time.Now()
		cs.StartAt(mime.MimeTypeOpus, false, now.Add(-duration))
		cs.UpdateMuteAt(false, now.Add(-1*time.Second))

		// Jitter does not knock quality down because it is dependent and hence not taken into account
		// at 2% loss, quality should stay at EXCELLENT purely based on loss. With high jitter (200 ms)
		// quality should drop to GOOD if jitter were taken into consideration
		trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
			1: {
				RTPStats: &rtpstats.RTPDeltaInfo{
					StartTime:   now,
					EndTime:     now.Add(duration),
					Packets:     250,
					PacketsLost: 5,
					JitterMax:   200,
				},
			},
		})
		cs.updateScoreAt(now.Add(duration))
		mos, quality := cs.GetScoreAndQuality()
		require.Greater(t, float32(4.6), mos)
		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, quality)
	})

	t.Run("codecs - packet", func(t *testing.T) {
		type expectedQuality struct {
			packetLossPercentage float64
			expectedMOS          float32
			expectedQuality      livekit.ConnectionQuality
		}
		testCases := []struct {
			name              string
			mimeType          mime.MimeType
			isFECEnabled      bool
			packetsExpected   uint32
			expectedQualities []expectedQuality
		}{
			// NOTE: Because of EWMA (Exponentially Weighted Moving Average), these cut off points are not exact
			// "audio/opus" - no fec - 0 <= loss < 2.5%: EXCELLENT, 2.5% <= loss < 7.5%: GOOD, >= 7.5%: POOR
			{
				name:            "audio/opus - no fec",
				mimeType:        mime.MimeTypeOpus,
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
						packetLossPercentage: 9.2,
						expectedMOS:          2.1,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
			// "audio/opus" - fec - 0 <= loss < 3.75%: EXCELLENT, 3.75% <= loss < 11.25%: GOOD, >= 11.25%: POOR
			{
				name:            "audio/opus - fec",
				mimeType:        mime.MimeTypeOpus,
				isFECEnabled:    true,
				packetsExpected: 200,
				expectedQualities: []expectedQuality{
					{
						packetLossPercentage: 3.0,
						expectedMOS:          4.6,
						expectedQuality:      livekit.ConnectionQuality_EXCELLENT,
					},
					{
						packetLossPercentage: 4.4,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 15.0,
						expectedMOS:          2.1,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
			// "audio/red" - no fec - 0 <= loss < 5%: EXCELLENT, 5% <= loss < 15%: GOOD, >= 15%: POOR
			{
				name:            "audio/red - no fec",
				mimeType:        mime.MimeTypeRED,
				isFECEnabled:    false,
				packetsExpected: 200,
				expectedQualities: []expectedQuality{
					{
						packetLossPercentage: 4.0,
						expectedMOS:          4.6,
						expectedQuality:      livekit.ConnectionQuality_EXCELLENT,
					},
					{
						packetLossPercentage: 6.0,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 19.5,
						expectedMOS:          2.1,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
			// "audio/red" - fec - 0 <= loss < 7.5%: EXCELLENT, 7.5% <= loss < 22.5%: GOOD, >= 22.5%: POOR
			{
				name:            "audio/red - fec",
				mimeType:        mime.MimeTypeRED,
				isFECEnabled:    true,
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
						packetLossPercentage: 30.0,
						expectedMOS:          2.1,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
			// "video/*" - 0 <= loss < 2%: EXCELLENT, 2% <= loss < 6%: GOOD, >= 6%: POOR
			{
				name:            "video/*",
				mimeType:        mime.MimeTypeVP8,
				isFECEnabled:    false,
				packetsExpected: 200,
				expectedQualities: []expectedQuality{
					{
						packetLossPercentage: 1.0,
						expectedMOS:          4.6,
						expectedQuality:      livekit.ConnectionQuality_EXCELLENT,
					},
					{
						packetLossPercentage: 3.5,
						expectedMOS:          4.1,
						expectedQuality:      livekit.ConnectionQuality_GOOD,
					},
					{
						packetLossPercentage: 8.0,
						expectedMOS:          2.1,
						expectedQuality:      livekit.ConnectionQuality_POOR,
					},
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				cs := NewConnectionStats(ConnectionStatsParams{
					IncludeRTT:       true,
					IncludeJitter:    true,
					ReceiverProvider: trp,
					Logger:           logger.GetLogger(),
				})

				duration := 5 * time.Second
				now := time.Now()
				cs.StartAt(tc.mimeType, tc.isFECEnabled, now.Add(-duration))

				for _, eq := range tc.expectedQualities {
					trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
						123: {
							RTPStats: &rtpstats.RTPDeltaInfo{
								StartTime:   now,
								EndTime:     now.Add(duration),
								Packets:     tc.packetsExpected,
								PacketsLost: uint32(math.Ceil(eq.packetLossPercentage * float64(tc.packetsExpected) / 100.0)),
							},
						},
					})
					cs.updateScoreAt(now.Add(duration))
					mos, quality := cs.GetScoreAndQuality()
					require.Greater(t, eq.expectedMOS, mos)
					require.Equal(t, eq.expectedQuality, quality)

					now = now.Add(duration)
				}
			})
		}
	})

	t.Run("bitrate", func(t *testing.T) {
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
			// ~2.7 <= expectedBits / actualBits < ~20.1 = GOOD
			// expectedBits / actualBits >= ~20.1 = POOR
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
				bytes:           uint64(math.Ceil(7_000_000.0 / 8.0 / 4.2)),
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
				bytes:           uint64(math.Ceil(8_000_000.0 / 8.0 / 75.0)),
				expectedMOS:     2.1,
				expectedQuality: livekit.ConnectionQuality_POOR,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				cs := NewConnectionStats(ConnectionStatsParams{
					IncludeRTT:         true,
					IncludeJitter:      true,
					EnableBitrateScore: true,
					ReceiverProvider:   trp,
					Logger:             logger.GetLogger(),
				})

				duration := 5 * time.Second
				now := time.Now()
				cs.StartAt(mime.MimeTypeVP8, false, now)

				for _, tr := range tc.transitions {
					cs.AddBitrateTransitionAt(tr.bitrate, now.Add(tr.offset))
				}

				trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
					123: {
						RTPStats: &rtpstats.RTPDeltaInfo{
							StartTime: now,
							EndTime:   now.Add(duration),
							Packets:   100,
							Bytes:     tc.bytes,
						},
					},
				})
				cs.updateScoreAt(now.Add(duration))
				mos, quality := cs.GetScoreAndQuality()
				require.Greater(t, tc.expectedMOS, mos)
				require.Equal(t, tc.expectedQuality, quality)
			})
		}
	})

	t.Run("layer", func(t *testing.T) {
		type transition struct {
			distance float64
			offset   time.Duration
		}
		testCases := []struct {
			name            string
			transitions     []transition
			expectedMOS     float32
			expectedQuality livekit.ConnectionQuality
		}{
			// NOTE: Because of EWMA (Exponentially Weighted Moving Average), these cut off points are not exact
			// each spatial layer missed drops o quality level
			{
				name: "excellent",
				transitions: []transition{
					{
						distance: 0.5,
					},
					{
						distance: 0.0,
						offset:   3 * time.Second,
					},
				},
				expectedMOS:     4.6,
				expectedQuality: livekit.ConnectionQuality_EXCELLENT,
			},
			{
				name: "good",
				transitions: []transition{
					{
						distance: 1.0,
					},
					{
						distance: 1.5,
						offset:   2 * time.Second,
					},
				},
				expectedMOS:     4.1,
				expectedQuality: livekit.ConnectionQuality_GOOD,
			},
			{
				name: "poor",
				transitions: []transition{
					{
						distance: 2.0,
					},
					{
						distance: 2.6,
						offset:   1 * time.Second,
					},
				},
				expectedMOS:     2.1,
				expectedQuality: livekit.ConnectionQuality_POOR,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				cs := NewConnectionStats(ConnectionStatsParams{
					IncludeRTT:       true,
					IncludeJitter:    true,
					ReceiverProvider: trp,
					Logger:           logger.GetLogger(),
				})

				duration := 5 * time.Second
				now := time.Now()
				cs.StartAt(mime.MimeTypeVP8, false, now)

				for _, tr := range tc.transitions {
					cs.AddLayerTransitionAt(tr.distance, now.Add(tr.offset))
				}

				trp.setStreams(map[uint32]*buffer.StreamStatsWithLayers{
					123: {
						RTPStats: &rtpstats.RTPDeltaInfo{
							StartTime: now,
							EndTime:   now.Add(duration),
							Packets:   200,
						},
					},
				})
				cs.updateScoreAt(now.Add(duration))
				mos, quality := cs.GetScoreAndQuality()
				require.Greater(t, tc.expectedMOS, mos)
				require.Equal(t, tc.expectedQuality, quality)
			})
		}
	})
}
