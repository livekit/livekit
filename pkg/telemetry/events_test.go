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
	clientMeta := &livekit.AnalyticsClientMeta{
		Region:            "dark-side",
		Node:              "moon",
		ClientAddr:        "127.0.0.1",
		ClientConnectTime: 420,
	}
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	guard := &telemetry.ReferenceGuard{}

	// do
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo, clientMeta, true, guard)
	time.Sleep(time.Millisecond * 500)

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

	require.Equal(t, clientMeta.Region, event.ClientMeta.Region)
	require.Equal(t, clientMeta.Node, event.ClientMeta.Node)
	require.Equal(t, clientMeta.ClientAddr, event.ClientMeta.ClientAddr)
	require.Equal(t, clientMeta.ClientConnectTime, event.ClientMeta.ClientConnectTime)
}

func Test_OnParticipantLeft_EventIsSent(t *testing.T) {
	fixture := createFixture()

	// prepare
	room := &livekit.Room{Sid: "RoomSid", Name: "RoomName"}
	partSID := "part1"
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	guard := &telemetry.ReferenceGuard{}

	// do
	fixture.sut.ParticipantActive(context.Background(), room, participantInfo, &livekit.AnalyticsClientMeta{}, false, guard)
	fixture.sut.ParticipantLeft(context.Background(), room, participantInfo, true, guard)
	time.Sleep(time.Millisecond * 500)

	// test
	require.Equal(t, 2, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(1)
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
	layer := &livekit.VideoLayer{
		Quality: livekit.VideoQuality_HIGH,
		Width:   uint32(360),
		Height:  uint32(720),
		Bitrate: 2048,
	}

	trackInfo := &livekit.TrackInfo{
		Sid:        trackID,
		Type:       livekit.TrackType_VIDEO,
		Muted:      false,
		Simulcast:  false,
		DisableDtx: false,
		Layers:     []*livekit.VideoLayer{layer},
	}

	// do
	fixture.sut.TrackPublishedUpdate(context.Background(), livekit.ParticipantID(partID), trackInfo)
	time.Sleep(time.Millisecond * 500)

	// test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)
	require.Equal(t, livekit.AnalyticsEventType_TRACK_PUBLISHED_UPDATE, event.Type)
	require.Equal(t, partID, event.ParticipantId)

	require.Equal(t, trackID, event.Track.Sid)
	require.NotNil(t, event.Track.Layers)
	require.Equal(t, layer.Width, event.Track.Layers[0].Width)
	require.Equal(t, layer.Height, event.Track.Layers[0].Height)
	require.Equal(t, layer.Quality, event.Track.Layers[0].Quality)

}

func Test_OnParticipantActive_EventIsSent(t *testing.T) {
	fixture := createFixture()

	// prepare participant to change status
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
	clientMeta := &livekit.AnalyticsClientMeta{
		Region:     "dark-side",
		Node:       "moon",
		ClientAddr: "127.0.0.1",
	}
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	guard := &telemetry.ReferenceGuard{}

	// do
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo, clientMeta, true, guard)
	time.Sleep(time.Millisecond * 500)

	// test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)

	// test
	// do
	clientMetaConnect := &livekit.AnalyticsClientMeta{
		ClientConnectTime: 420,
	}

	fixture.sut.ParticipantActive(context.Background(), room, participantInfo, clientMetaConnect, false, guard)
	time.Sleep(time.Millisecond * 500)

	require.Equal(t, 2, fixture.analytics.SendEventCallCount())
	_, eventActive := fixture.analytics.SendEventArgsForCall(1)
	require.Equal(t, livekit.AnalyticsEventType_PARTICIPANT_ACTIVE, eventActive.Type)
	require.Equal(t, partSID, eventActive.ParticipantId)
	require.Equal(t, room.Sid, eventActive.RoomId)
	require.Equal(t, room, event.Room)

	require.Equal(t, clientMetaConnect.ClientConnectTime, eventActive.ClientMeta.ClientConnectTime)
}

func Test_OnTrackSubscribed_EventIsSent(t *testing.T) {
	fixture := createFixture()

	// prepare participant to change status
	room := &livekit.Room{Sid: "RoomSid", Name: "RoomName"}
	partSID := "part1"
	publisherInfo := &livekit.ParticipantInfo{Sid: "pub1", Identity: "publisher"}
	trackInfo := &livekit.TrackInfo{Sid: "tr1", Type: livekit.TrackType_VIDEO}

	clientInfo := &livekit.ClientInfo{
		Sdk:            2,
		Version:        "v1",
		Os:             "mac",
		OsVersion:      "v1",
		DeviceModel:    "DM1",
		Browser:        "chrome",
		BrowserVersion: "97.0.1",
	}
	clientMeta := &livekit.AnalyticsClientMeta{
		Region:     "dark-side",
		Node:       "moon",
		ClientAddr: "127.0.0.1",
	}
	participantInfo := &livekit.ParticipantInfo{Sid: partSID}
	guard := &telemetry.ReferenceGuard{}

	// do
	fixture.sut.ParticipantJoined(context.Background(), room, participantInfo, clientInfo, clientMeta, true, guard)
	time.Sleep(time.Millisecond * 500)

	// test
	require.Equal(t, 1, fixture.analytics.SendEventCallCount())
	_, event := fixture.analytics.SendEventArgsForCall(0)
	require.Equal(t, room, event.Room)

	// do
	fixture.sut.TrackSubscribed(context.Background(), livekit.ParticipantID(partSID), trackInfo, publisherInfo, true)
	time.Sleep(time.Millisecond * 500)

	require.Eventually(t, func() bool {
		return fixture.analytics.SendEventCallCount() == 2
	}, time.Second, time.Millisecond*50, "expected send event to be called twice")
	_, eventTrackSubscribed := fixture.analytics.SendEventArgsForCall(1)
	require.Equal(t, livekit.AnalyticsEventType_TRACK_SUBSCRIBED, eventTrackSubscribed.Type)
	require.Equal(t, partSID, eventTrackSubscribed.ParticipantId)
	require.Equal(t, trackInfo.Sid, eventTrackSubscribed.Track.Sid)
	require.Equal(t, trackInfo.Type, eventTrackSubscribed.Track.Type)
	require.Equal(t, publisherInfo.Sid, eventTrackSubscribed.Publisher.Sid)
	require.Equal(t, publisherInfo.Identity, eventTrackSubscribed.Publisher.Identity)

}
