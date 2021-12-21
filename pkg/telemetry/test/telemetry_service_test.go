package telemetrytest

import (
	"context"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"

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

func Test_OnDownstreamPacket(t *testing.T) {
	fixture := createFixture()

	//prepare
	room := &livekit.Room{}
	partSID := "part1"
	clientInfo := &livekit.ClientInfo{Sdk: 2}
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo)

	//do
	packets := []int{33, 23}
	totalBytes := packets[0] + packets[1]
	totalPackets := len(packets)
	for i := range packets {
		fixture.sut.OnDownstreamPacket(partSID, packets[i])
	}
	fixture.sut.SendAnalytics()

	//test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, totalBytes, int(stats[0].TotalBytes))
	require.Equal(t, totalPackets, int(stats[0].TotalPackets))
}

func Test_AnalyticsSentWhenParticipantLeaves(t *testing.T) {
	fixture := createFixture()

	//prepare
	room := &livekit.Room{}
	partSID := "part1"
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo)

	//do
	fixture.sut.ParticipantLeft(context.Background(), room, participantInfo)

	//test
	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
}
