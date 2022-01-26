package telemetrytest

import (
	"context"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
)

type telemetryServiceFixture struct {
	sut       telemetry.TelemetryServiceInternal
	analytics *telemetryfakes.FakeAnalyticsService
}

func createFixture() *telemetryServiceFixture {
	fixture := &telemetryServiceFixture{}
	fixture.analytics = &telemetryfakes.FakeAnalyticsService{}
	fixture.sut = telemetry.NewTelemetryServiceInternal(nil, fixture.analytics)
	return fixture
}

func Test_ParticipantAndRoomDataAreSentWithAnalytics(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{Sid: "RoomSid", Name: "RoomName"}
	partSID := livekit.ParticipantID("part1")
	clientInfo := &livekit.ClientInfo{Sdk: 2}
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo)

	// do
	packet := 33
	stat := &livekit.AnalyticsStat{TotalBytes: uint64(packet)}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, "", stat)
	fixture.sut.SendAnalytics()

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
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo)

	// do
	packets := []int{33, 23}
	totalBytes := packets[0] + packets[1]
	totalPackets := len(packets)
	trackID := livekit.TrackID("trackID")
	for i := range packets {
		stat := &livekit.AnalyticsStat{TotalBytes: uint64(packets[i])}
		fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID, stat)
	}
	fixture.sut.SendAnalytics()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, totalBytes, int(stats[0].TotalBytes))
	require.Equal(t, totalPackets, int(stats[0].TotalPackets))
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_OnDownstreamPackets_SeveralTracks(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	clientInfo := &livekit.ClientInfo{Sdk: 2}
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo)

	// do
	packet1 := 33
	trackID1 := livekit.TrackID("trackID1")
	stat1 := &livekit.AnalyticsStat{TotalPackets: uint64(uint32(packet1))}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID1, stat1)

	packet2 := 23
	trackID2 := livekit.TrackID("trackID2")
	stat2 := &livekit.AnalyticsStat{TotalPackets: uint64(uint32(packet2))}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID2, stat2)
	fixture.sut.SendAnalytics()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 2, len(stats))

	found1 := false
	found2 := false
	for _, sentStat := range stats {
		if livekit.TrackID(sentStat.TrackId) == trackID1 {
			found1 = true
			require.Equal(t, packet1, int(sentStat.TotalBytes))
			require.Equal(t, 1, int(sentStat.TotalPackets))
		} else if livekit.TrackID(sentStat.TrackId) == trackID2 {
			found2 = true
			require.Equal(t, packet2, int(sentStat.TotalBytes))
			require.Equal(t, 1, int(sentStat.TotalPackets))
		}
	}
	require.True(t, found1)
	require.True(t, found2)
}

func Test_OnDownStreamRTCP(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)

	// do
	stat1 := &livekit.AnalyticsStat{Jitter: 5, PacketLost: 3, TotalBytes: 1}
	trackID := livekit.TrackID("trackID1")
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID, stat1)
	stat2 := &livekit.AnalyticsStat{Jitter: 2, PacketLost: 4}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID, stat2)

	fixture.sut.SendAnalytics()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, 1, int(stats[0].NackCount))
	require.Equal(t, 1, int(stats[0].PliCount))
	require.Equal(t, 1, int(stats[0].FirCount))
	require.Equal(t, 0, int(stats[0].Rtt))        // TODO: test for RTT
	require.Equal(t, 5, int(stats[0].Jitter))     // max of jitter, see list of rtcp.ReceptionReport above
	require.Equal(t, 4, int(stats[0].PacketLost)) // last reported packets lost, see list of rtcp.ReceptionReport above
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_PacketLostDiffShouldBeSentToTelemetry(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)

	// do
	trackID := livekit.TrackID("trackID1")
	stat1 := &livekit.AnalyticsStat{PacketLost: 1}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID, stat1) // there should be bytes reported so that stats are sent
	fixture.sut.SendAnalytics()
	stat2 := &livekit.AnalyticsStat{PacketLost: 4}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID, stat2)
	fixture.sut.SendAnalytics()

	// test
	require.Equal(t, 2, fixture.analytics.SendStatsCallCount()) // 2 calls to fixture.sut.SendAnalytics()
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, 1, int(stats[0].PacketLost)) // see pkts1

	_, stats = fixture.analytics.SendStatsArgsForCall(1)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, 3, int(stats[0].PacketLost)) // see diff of TotalLost between pkts2 and pkts1
}

func Test_OnDownStreamRTCP_SeveralTracks(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)

	// do

	trackID1 := livekit.TrackID("trackID1")
	trackID2 := livekit.TrackID("trackID2")
	stat1 := &livekit.AnalyticsStat{TotalBytes: 1}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID1, stat1) // there should be bytes reported so that stats are sent

	stat2 := &livekit.AnalyticsStat{NackCount: 1}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID1, stat2)

	stat3 := &livekit.AnalyticsStat{FirCount: 1}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, trackID2, stat3)
	fixture.sut.SendAnalytics()

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
			require.Equal(t, 1, int(sentStat.NackCount)) // see pkts1 above
		} else if livekit.TrackID(sentStat.TrackId) == trackID2 {
			found2 = true
			require.Equal(t, livekit.StreamType_DOWNSTREAM, sentStat.Kind)
			require.Equal(t, 1, int(sentStat.FirCount)) // see pkts2 above
		}
	}
	require.True(t, found1)
	require.True(t, found2)
}
func Test_OnUpstreamRTCP(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)

	// do

	stat1 := &livekit.AnalyticsStat{NackCount: 1, PliCount: 1, FirCount: 1, Jitter: 5, PacketLost: 3, TotalPackets: 1, TotalBytes: 1}
	trackID := livekit.TrackID("trackID")

	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID, stat1)

	stat2 := &livekit.AnalyticsStat{Jitter: 2, PacketLost: 4}
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID, stat2)
	fixture.sut.SendAnalytics()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_UPSTREAM, stats[0].Kind)
	require.Equal(t, 1, int(stats[0].NackCount))
	require.Equal(t, 1, int(stats[0].PliCount))
	require.Equal(t, 1, int(stats[0].FirCount))
	require.Equal(t, 0, int(stats[0].Rtt))        // TODO: test for RTT
	require.Equal(t, 5, int(stats[0].Jitter))     // max of jitter, see list of rtcp.ReceptionReport above
	require.Equal(t, 4, int(stats[0].PacketLost)) // last reported packets lost, see list of rtcp.ReceptionReport above
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_OnUpstreamRTCP_SeveralTracks(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)

	// there should be bytes reported so that stats are sent
	totalBytes := 1
	totalPackets := 1
	trackID1 := livekit.TrackID("trackID1")
	trackID2 := livekit.TrackID("trackID2")

	stat1 := &livekit.AnalyticsStat{TotalBytes: uint64(totalBytes), TotalPackets: uint64(totalPackets)}
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID1, stat1)
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID2, stat1) // using same buffer is not correct but for test it is fine

	// do
	stat2 := &livekit.AnalyticsStat{NackCount: 1}
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID1, stat2)

	stat3 := &livekit.AnalyticsStat{FirCount: 1}
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID2, stat3)
	fixture.sut.SendAnalytics()

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
			require.Equal(t, 1, int(sentStat.NackCount)) // see pkts1 above
		} else if livekit.TrackID(sentStat.TrackId) == trackID2 {
			found2 = true
			require.Equal(t, livekit.StreamType_UPSTREAM, sentStat.Kind)
			require.Equal(t, 1, int(sentStat.FirCount)) // see pkts2 above
		}
		require.Equal(t, totalBytes, int(sentStat.TotalBytes))
		require.Equal(t, totalPackets, int(sentStat.TotalPackets))
	}
	require.True(t, found1)
	require.True(t, found2)

	// remove 1 buffer
	fixture.sut.TrackUnpublished(context.Background(), partSID, &livekit.TrackInfo{Sid: string(trackID2)}, 0)
	fixture.sut.SendAnalytics()
	require.Equal(t, 2, fixture.analytics.SendStatsCallCount())
	_, stats = fixture.analytics.SendStatsArgsForCall(1)
	require.Equal(t, 2, len(stats)) // still 2 tracks, next call won't contain 1 track

	// now only 1 track stats remaining
	fixture.sut.SendAnalytics()
	require.Equal(t, 3, fixture.analytics.SendStatsCallCount())
	_, stats = fixture.analytics.SendStatsArgsForCall(2)
	require.Equal(t, 1, len(stats)) // now only 1 track remaining
}

func Test_AnalyticsSentWhenParticipantLeaves(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := "part1"
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)

	// do
	fixture.sut.ParticipantLeft(context.Background(), room, participantInfo)

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
}

func Test_AddUpTrack(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)

	// do
	var totalBytes uint64 = 3
	var totalPackets uint32 = 3

	stat := &livekit.AnalyticsStat{TotalPackets: uint64(totalPackets), TotalBytes: totalBytes}
	trackID := livekit.TrackID("trackID")
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID, stat)
	fixture.sut.SendAnalytics()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_UPSTREAM, stats[0].Kind)
	require.Equal(t, totalBytes, stats[0].TotalBytes)
	require.Equal(t, totalPackets, uint32(stats[0].TotalPackets))
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_AddUpTrack_SeveralBuffers_Simulcast(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)
	// do
	trackID := livekit.TrackID("trackID")

	stat1 := &livekit.AnalyticsStat{TotalBytes: 1, TotalPackets: 1}
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID, stat1)
	// buffer 2
	buf2 := &buffer.Buffer{}
	buf2.SetStatsTestOnly(buffer.Stats{
		PacketCount: 2,
		TotalByte:   2,
	})

	stat2 := &livekit.AnalyticsStat{TotalPackets: 2, TotalBytes: 2}
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, trackID, stat2)
	fixture.sut.SendAnalytics()
	// test
	totalBytes := stat1.TotalBytes + stat2.TotalBytes
	totalPackets := stat1.TotalPackets + stat2.TotalPackets
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_UPSTREAM, stats[0].Kind)
	require.Equal(t, totalBytes, stats[0].TotalBytes)
	require.Equal(t, totalPackets, uint32(stats[0].TotalPackets))
	require.Equal(t, string(trackID), stats[0].TrackId)
}

func Test_BothDownstreamAndUpstreamStatsAreSentTogether(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{}
	partSID := livekit.ParticipantID("part1")
	participantInfo := &livekit.ParticipantInfo{Sid: string(partSID)}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, nil)

	// do
	// upstream bytes
	buf := &buffer.Buffer{}
	buf.SetStatsTestOnly(buffer.Stats{
		PacketCount: 3,
		TotalByte:   3,
	})
	stat1 := &livekit.AnalyticsStat{TotalPackets: 3, TotalBytes: 3}
	fixture.sut.TrackStats(livekit.StreamType_UPSTREAM, partSID, "trackID", stat1)
	// downstream bytes
	stat2 := &livekit.AnalyticsStat{TotalPackets: 1, TotalBytes: 1}
	fixture.sut.TrackStats(livekit.StreamType_DOWNSTREAM, partSID, "trackID1", stat2)
	fixture.sut.SendAnalytics()

	// test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 2, len(stats))
	require.Equal(t, livekit.StreamType_UPSTREAM, stats[0].Kind)
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[1].Kind)
}
