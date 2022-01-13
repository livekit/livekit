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
	clientInfo := &livekit.ClientInfo{
		Sdk:            2,
		Version:        "v1",
		Os:             "mac",
		OsVersion:      "v1",
		DeviceModel:    "DM1",
		Browser:        "chrome",
		BrowserVersion: "97.0.1",
	}
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}

	//do
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo)

	//test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)
	require.Equal(t, livekit.AnalyticsEventType_PARTICIPANT_JOINED, event.Type)
	require.Equal(t, partSID, event.ParticipantId)
	require.Equal(t, participantInfo, event.Participant)
	require.Equal(t, room.Sid, event.RoomId)
	require.Equal(t, room, event.Room)
	require.Equal(t, clientInfo.Sdk, event.SdkType)
	require.Equal(t, clientInfo.Version, event.ClientVersion)
	require.Equal(t, clientInfo.Os, event.ClientOs)
	require.Equal(t, clientInfo.OsVersion, event.ClientOsVersion)
	require.Equal(t, clientInfo.DeviceModel, event.ClientDeviceModel)
	require.Equal(t, clientInfo.Browser, event.ClientBrowser)
	require.Equal(t, clientInfo.BrowserVersion, event.ClientBrowserVersion)
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
	require.Equal(t, room.Sid, event.RoomId)
	require.Equal(t, room, event.Room)
}
