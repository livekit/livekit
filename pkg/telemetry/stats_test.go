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

package telemetry_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
)

func init() {
	prometheus.Init("test", livekit.NodeType_SERVER)
}

type telemetryServiceFixture struct {
	sut       telemetry.TelemetryService
	analytics *telemetryfakes.FakeAnalyticsService
}

func createFixture() *telemetryServiceFixture {
	fixture := &telemetryServiceFixture{}
	fixture.analytics = &telemetryfakes.FakeAnalyticsService{}
	fixture.sut = telemetry.NewTelemetryService(nil, fixture.analytics)
	return fixture
}

func Test_ParticipantAndRoomDataAreSentWithAnalytics(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{Sid: "RoomSid", Name: "RoomName"}
	partSID := livekit.ParticipantID("part1")
	clientInfo := &livekit.ClientInfo{Sdk: 2}
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo, nil, true, guard)

	// do
	packet := 33
	stat := &livekit.AnalyticsStat{Streams: []*livekit.AnalyticsStream{{PrimaryBytes: uint64(packet)}}}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, ""), stat)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, string(partSID), stats[0].ParticipantId)
	require.Equal(t, room.Sid, stats[0].RoomId)
	require.Equal(t, room.Name, stats[0].RoomName)
}

func Test_OnDownstreamPackets(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	clientInfo := &livekit.ClientInfo{Sdk: 2}
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo, nil, true, guard)

	// do
	packets := []int{33, 23}
	totalBytes := packets[0] + packets[1]
	totalPackets := len(packets)
	trackID := livekit.TrackID("trackID")
	for i := range packets {
		stat := &livekit.AnalyticsStat{Streams: []*livekit.AnalyticsStream{{PrimaryBytes: uint64(packets[i]), PrimaryPackets: uint32(1)}}}
		fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID), stat)
	}

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, totalBytes, int(stats[0].Streams[0].PrimaryBytes))
	require.Equal(t, totalPackets, int(stats[0].Streams[0].PrimaryPackets))
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_OnDownstreamPackets_SeveralTracks(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	clientInfo := &livekit.ClientInfo{Sdk: 2}
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo, nil, true, guard)

	// do
	packet1 := 33
	trackID1 := livekit.TrackID("trackID1")
	stat1 := &livekit.AnalyticsStat{Streams: []*livekit.AnalyticsStream{{PrimaryBytes: uint64(packet1), PrimaryPackets: 1}}}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID1), stat1)

	packet2 := 23
	trackID2 := livekit.TrackID("trackID2")
	stat2 := &livekit.AnalyticsStat{Streams: []*livekit.AnalyticsStream{{PrimaryBytes: uint64(packet2), PrimaryPackets: 1}}}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID2), stat2)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 2, len(stats))

	found1 := false
	found2 := false
	for _, sentStat := range stats {
		if livekit.TrackID(sentStat.TrackId) == trackID1 {
			found1 = true
			require.Equal(t, packet1, int(sentStat.Streams[0].PrimaryBytes))
			require.Equal(t, 1, int(sentStat.Streams[0].PrimaryPackets))
		} else if livekit.TrackID(sentStat.TrackId) == trackID2 {
			found2 = true
			require.Equal(t, packet2, int(sentStat.Streams[0].PrimaryBytes))
			require.Equal(t, 1, int(sentStat.Streams[0].PrimaryPackets))
		}
	}
	require.True(t, found1)
	require.True(t, found2)
}

func Test_OnDownStreamStat(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// do
	stat1 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   1,
				PrimaryPackets: 1,
				PacketsLost:    3,
				Nacks:          1,
				Plis:           1,
				Rtt:            23,
				Jitter:         3,
			},
		},
	}
	trackID := livekit.TrackID("trackID1")
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID), stat1)

	stat2 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   2,
				PrimaryPackets: 2,
				PacketsLost:    4,
				Nacks:          1,
				Plis:           1,
				Firs:           1,
				Rtt:            10,
				Jitter:         5,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID), stat2)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, 2, int(stats[0].Streams[0].Nacks))
	require.Equal(t, 2, int(stats[0].Streams[0].Plis))
	require.Equal(t, 1, int(stats[0].Streams[0].Firs))
	require.Equal(t, 23, int(stats[0].Streams[0].Rtt))        // max of RTT
	require.Equal(t, 5, int(stats[0].Streams[0].Jitter))      // max of jitter
	require.Equal(t, 7, int(stats[0].Streams[0].PacketsLost)) // coalesced delta packet losses
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_PacketLostDiffShouldBeSentToTelemetry(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// do
	trackID := livekit.TrackID("trackID1")
	stat1 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   1,
				PrimaryPackets: 1,
				PacketsLost:    1,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID), stat1) // there should be bytes reported so that stats are sent

	// flush
	fixture.flush()

	stat2 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   2,
				PrimaryPackets: 2,
				PacketsLost:    4,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID), stat2)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 2, fixture.analytics.SendStatsCallCount()) // 2 calls to fixture.sut.FlushStats()
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, 1, int(stats[0].Streams[0].PacketsLost)) // see pkts1

	_, stats = fixture.analytics.SendStatsArgsForCall(1)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, 4, int(stats[0].Streams[0].PacketsLost)) // delta loss should be sent as is
}

func Test_OnDownStreamRTCP_SeveralTracks(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// do
	trackID1 := livekit.TrackID("trackID1")
	trackID2 := livekit.TrackID("trackID2")
	stat1 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   1,
				PrimaryPackets: 1,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID1), stat1) // there should be bytes reported so that stats are sent

	stat2 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   2,
				PrimaryPackets: 2,
				Nacks:          1,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID1), stat2)

	stat3 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   3,
				PrimaryPackets: 3,
				Firs:           1,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, trackID2), stat3)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 2, len(stats))

	found1 := false
	found2 := false
	for _, sentStat := range stats {
		if livekit.TrackID(sentStat.TrackId) == trackID1 {
			found1 = true
			require.Equal(t, livekit.StreamType_DOWNSTREAM, sentStat.Kind)
			require.Equal(t, 1, int(sentStat.Streams[0].Nacks)) // see pkts1 above
		} else if livekit.TrackID(sentStat.TrackId) == trackID2 {
			found2 = true
			require.Equal(t, livekit.StreamType_DOWNSTREAM, sentStat.Kind)
			require.Equal(t, 1, int(sentStat.Streams[0].Firs)) // see pkts2 above
		}
	}
	require.True(t, found1)
	require.True(t, found2)
}

func Test_OnUpstreamStat(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// do
	stat1 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   1,
				PrimaryPackets: 1,
				PacketsLost:    3,
				Nacks:          1,
				Plis:           1,
				Firs:           1,
				Rtt:            13,
				Jitter:         5,
			},
		},
	}
	trackID := livekit.TrackID("trackID")

	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, trackID), stat1)

	stat2 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   2,
				PrimaryPackets: 2,
				PacketsLost:    4,
				Nacks:          1,
				Plis:           1,
				Firs:           1,
				Rtt:            33,
				Jitter:         2,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, trackID), stat2)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_UPSTREAM, stats[0].Kind)
	require.Equal(t, 2, int(stats[0].Streams[0].Nacks))
	require.Equal(t, 2, int(stats[0].Streams[0].Plis))
	require.Equal(t, 2, int(stats[0].Streams[0].Firs))
	require.Equal(t, 33, int(stats[0].Streams[0].Rtt))        // max of RTT
	require.Equal(t, 5, int(stats[0].Streams[0].Jitter))      // max of jitter
	require.Equal(t, 7, int(stats[0].Streams[0].PacketsLost)) // coalesced delta packet losses
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_OnUpstreamRTCP_SeveralTracks(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	identity := livekit.ParticipantIdentity("part1Identity")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID), Identity: string(identity)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// there should be bytes reported so that stats are sent
	totalBytes := 1
	totalPackets := 1
	trackID1 := livekit.TrackID("trackID1")
	trackID2 := livekit.TrackID("trackID2")

	stat1 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   uint64(totalBytes),
				PrimaryPackets: uint32(totalPackets),
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, trackID1), stat1)
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, trackID2), stat1) // using same buffer is not correct but for test it is fine

	// do
	totalBytes++
	totalPackets++
	stat2 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   uint64(totalBytes),
				PrimaryPackets: uint32(totalPackets),
				Nacks:          1,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, trackID1), stat2)

	stat3 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   uint64(totalBytes),
				PrimaryPackets: uint32(totalPackets),
				Firs:           1,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, trackID2), stat3)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 2, len(stats))

	found1 := false
	found2 := false
	for _, sentStat := range stats {
		if livekit.TrackID(sentStat.TrackId) == trackID1 {
			found1 = true
			require.Equal(t, livekit.StreamType_UPSTREAM, sentStat.Kind)
			require.Equal(t, 1, int(sentStat.Streams[0].Nacks)) // see pkts1 above
		} else if livekit.TrackID(sentStat.TrackId) == trackID2 {
			found2 = true
			require.Equal(t, livekit.StreamType_UPSTREAM, sentStat.Kind)
			require.Equal(t, 1, int(sentStat.Streams[0].Firs)) // see pkts2 above
		}
		require.Equal(t, 3, int(sentStat.Streams[0].PrimaryBytes))
		require.Equal(t, 3, int(sentStat.Streams[0].PrimaryPackets))
	}
	require.True(t, found1)
	require.True(t, found2)

	// remove 1 track - track stats were flushed above, so no more calls to SendStats
	fixture.sut.TrackUnpublished(context.Background(), partSID, identity, &livekit.TrackInfo{Sid: string(trackID2)}, true)

	// flush
	fixture.flush()

	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
}

func Test_AnalyticsSentWhenParticipantLeaves(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := "part1"
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// do
	fixture.sut.ParticipantLeft(context.Background(), room, participantInfo, true, guard)

	// should not be called if there are no track stats
	time.Sleep(time.Millisecond * 500)
	require.Equal(t, 0, fixture.analytics.SendStatsCallCount())
}

func Test_AddUpTrack(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// do
	var totalBytes uint64 = 3
	var totalPackets uint32 = 3

	stat := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   totalBytes,
				PrimaryPackets: totalPackets,
			},
		},
	}
	trackID := livekit.TrackID("trackID")
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, trackID), stat)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_UPSTREAM, stats[0].Kind)
	require.Equal(t, totalBytes, stats[0].Streams[0].PrimaryBytes)
	require.Equal(t, totalPackets, stats[0].Streams[0].PrimaryPackets)
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_AddUpTrack_SeveralBuffers_Simulcast(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// do
	trackID := livekit.TrackID("trackID")
	stat1 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   1,
				PrimaryPackets: 1,
			},
			{
				PrimaryBytes:   2,
				PrimaryPackets: 2,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, trackID), stat1)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_UPSTREAM, stats[0].Kind)
	// should be a consolidated stream
	require.Equal(t, stat1.Streams[0].PrimaryBytes+stat1.Streams[1].PrimaryBytes, stats[0].Streams[0].PrimaryBytes)
	require.Equal(t, stat1.Streams[0].PrimaryPackets+stat1.Streams[1].PrimaryPackets, stats[0].Streams[0].PrimaryPackets)
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_BothDownstreamAndUpstreamStatsAreSentTogether(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	guard := &telemetry.ReferenceGuard{}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil, nil, true, guard)

	// do
	// upstream bytes
	stat1 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   3,
				PrimaryPackets: 3,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_UPSTREAM, partSID, "trackID"), stat1)
	// downstream bytes
	stat2 := &livekit.AnalyticsStat{
		Streams: []*livekit.AnalyticsStream{
			{
				PrimaryBytes:   1,
				PrimaryPackets: 1,
			},
		},
	}
	fixture.sut.TrackStats(telemetry.StatsKeyForData("test", livekit.StreamType_DOWNSTREAM, partSID, "trackID1"), stat2)

	// flush
	fixture.flush()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 2, len(stats))
	require.Equal(t, livekit.StreamType_UPSTREAM, stats[0].Kind)
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[1].Kind)
}

func (f *telemetryServiceFixture) flush() {
	time.Sleep(time.Millisecond * 500)
	f.sut.FlushStats()
}
