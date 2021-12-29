package telemetrytest

import (
	"context"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func Test_OnParticipantJoin_EventIsSent(t *testing.T) {
	fixture := createFixture()

	//prepare
	room := &livekit.Room{Sid: "RoomSid", Name: "RoomName"}
	partSID := "part1"
	clientInfo := &livekit.ClientInfo{Sdk: 2}
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}

	//do
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo)

	//test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)
	require.Equal(t, livekit.AnalyticsEventType_PARTICIPANT_JOINED, event.Type)
	require.Equal(t, partSID, event.ParticipantId)
	require.Equal(t, participantInfo, event.Participant)
	require.Equal(t, room.Sid, event.RoomSid)
	require.Equal(t, room, event.Room)
	require.Equal(t, clientInfo.Sdk, event.SdkType)
}

func Test_OnParticipantLeft_EventIsSent(t *testing.T) {
	fixture := createFixture()

	//prepare
	room := &livekit.Room{Sid: "RoomSid", Name: "RoomName"}
	partSID := "part1"
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}

	//do
	fixture.sut.ParticipantLeft(context.Background(), room, participantInfo)

	//test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)
	require.Equal(t, livekit.AnalyticsEventType_PARTICIPANT_LEFT, event.Type)
	require.Equal(t, partSID, event.ParticipantId)
	require.Equal(t, room.Sid, event.RoomSid)
	require.Equal(t, room, event.Room)
}
