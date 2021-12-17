package telemetrytest

import (
	"context"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
)

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

func Test_TelemetryService_Downstream_Stats(t *testing.T) {
	fixture := createFixture()

	room := &livekit.Room{}
	partSID := "part1"
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo)
	totalBytes := 33
	fixture.sut.OnDownstreamPacket(partSID, totalBytes)

	// call participant left to trigget sending of analytics
	fixture.sut.ParticipantLeft(context.Background(), room, participantInfo)

	time.Sleep(time.Millisecond * 100) // wait for Update function to be called in go routine

	require.Equal(t, 1, fixture.analytics.SendStatsCallCount())
	_, stats := fixture.analytics.SendStatsArgsForCall(0)
	require.Equal(t, 1, len(stats))
	require.Equal(t, livekit.StreamType_DOWNSTREAM, stats[0].Kind)
	require.Equal(t, totalBytes, int(stats[0].TotalBytes))
}
