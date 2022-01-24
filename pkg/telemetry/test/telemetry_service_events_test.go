package telemetrytest

import (
	"context"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func Test_OnParticipantJoin_EventIsSent(t *testing.T) {
	fixture := createFixture()

	// prepare
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

	// do
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo)

	// test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)
	require.Equal(t, livekit.AnalyticsEventType_PARTICIPANT_JOINED, event.Type)
	require.Equal(t, partSID, event.ParticipantId)
	require.Equal(t, participantInfo, event.Participant)
	require.Equal(t, room.Sid, event.RoomId)
	require.Equal(t, room, event.Room)

	require.Equal(t, clientInfo.Sdk, event.ClientInfo.Sdk)
	require.Equal(t, clientInfo.Version, event.ClientInfo.Version)
	require.Equal(t, clientInfo.Os, event.ClientInfo.Os)
	require.Equal(t, clientInfo.OsVersion, event.ClientInfo.OsVersion)
	require.Equal(t, clientInfo.DeviceModel, event.ClientInfo.DeviceModel)
	require.Equal(t, clientInfo.Browser, event.ClientInfo.Browser)
	require.Equal(t, clientInfo.BrowserVersion, event.ClientInfo.BrowserVersion)
}

func Test_OnParticipantLeft_EventIsSent(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{Sid: "RoomSid", Name: "RoomName"}
	partSID := "part1"
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}

	// do
	fixture.sut.ParticipantLeft(context.Background(), room, participantInfo)

	// test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)
	require.Equal(t, livekit.AnalyticsEventType_PARTICIPANT_LEFT, event.Type)
	require.Equal(t, partSID, event.ParticipantId)
	require.Equal(t, room.Sid, event.RoomId)
	require.Equal(t, room, event.Room)
}

func Test_OnTrackUpdate_EventIsSent(t *testing.T) {
	fixture := createFixture()

	// prepare
	partID := "part1"
	trackID := "track1"
	width := uint32(360)
	height := uint32(720)
	trackInfo := &livekit.TrackInfo{
		Sid:        trackID,
		Type:       livekit.TrackType_VIDEO,
		Muted:      false,
		Width:      width,
		Height:     height,
		Simulcast:  false,
		DisableDtx: false,
	}

	// do
	fixture.sut.TrackPublishedUpdate(context.Background(), livekit.ParticipantID(partID), trackInfo)

	// test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)
	require.Equal(t, livekit.AnalyticsEventType_TRACK_PUBLISHED_UPDATE, event.Type)
	require.Equal(t, partID, event.ParticipantId)

	require.Equal(t, trackID, event.Track.Sid)
	require.Equal(t, width, event.Track.Width)
	require.Equal(t, height, event.Track.Height)

}
