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

package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
)

// a scenario with lots of clients connecting, publishing, and leaving at random periods
func scenarioPublishingUponJoining(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("puj_1", defaultServerPort, useSinglePeerConnection, nil)
			c2 := createRTCClient("puj_2", secondServerPort, useSinglePeerConnection, &testclient.Options{AutoSubscribe: true})
			c3 := createRTCClient("puj_3", defaultServerPort, useSinglePeerConnection, &testclient.Options{AutoSubscribe: true})
			defer stopClients(c1, c2, c3)

			waitUntilConnected(t, c1, c2, c3)

			// c1 and c2 publishing, c3 just receiving
			writers := publishTracksForClients(t, c1, c2)
			defer stopWriters(writers...)

			logger.Infow("waiting to receive tracks from c1 and c2")
			testutils.WithTimeout(t, func() string {
				tracks := c3.SubscribedTracks()
				if len(tracks[c1.ID()]) != 2 {
					return "did not receive tracks from c1"
				}
				if len(tracks[c2.ID()]) != 2 {
					return "did not receive tracks from c2"
				}
				return ""
			})

			// after a delay, c2 reconnects, then publishing
			time.Sleep(syncDelay)
			c2.Stop()

			logger.Infow("waiting for c2 tracks to be gone")
			testutils.WithTimeout(t, func() string {
				tracks := c3.SubscribedTracks()

				if len(tracks[c1.ID()]) != 2 {
					return fmt.Sprintf("c3 should be subscribed to 2 tracks from c1, actual: %d", len(tracks[c1.ID()]))
				}
				if len(tracks[c2.ID()]) != 0 {
					return fmt.Sprintf("c3 should be subscribed to 0 tracks from c2, actual: %d", len(tracks[c2.ID()]))
				}
				if len(c1.SubscribedTracks()[c2.ID()]) != 0 {
					return fmt.Sprintf("c3 should be subscribed to 0 tracks from c2, actual: %d", len(c1.SubscribedTracks()[c2.ID()]))
				}
				return ""
			})

			logger.Infow("c2 reconnecting")
			// connect to a diff port
			c2 = createRTCClient("puj_2", defaultServerPort, useSinglePeerConnection, nil)
			defer c2.Stop()
			waitUntilConnected(t, c2)
			writers = publishTracksForClients(t, c2)
			defer stopWriters(writers...)

			testutils.WithTimeout(t, func() string {
				tracks := c3.SubscribedTracks()
				// "new c2 tracks should be published again",
				if len(tracks[c2.ID()]) != 2 {
					return fmt.Sprintf("c3 should be subscribed to 2 tracks from c2, actual: %d", len(tracks[c2.ID()]))
				}
				if len(c1.SubscribedTracks()[c2.ID()]) != 2 {
					return fmt.Sprintf("c1 should be subscribed to 2 tracks from c2, actual: %d", len(c1.SubscribedTracks()[c2.ID()]))
				}
				return ""
			})
		})
	}
}

func scenarioReceiveBeforePublish(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("rbp_1", defaultServerPort, useSinglePeerConnection, nil)
			c2 := createRTCClient("rbp_2", defaultServerPort, useSinglePeerConnection, nil)

			waitUntilConnected(t, c1, c2)
			defer stopClients(c1, c2)

			// c1 publishes
			writers := publishTracksForClients(t, c1)
			defer stopWriters(writers...)

			// c2 should see some bytes flowing through
			testutils.WithTimeout(t, func() string {
				if c2.BytesReceived() > 20 {
					return ""
				} else {
					return fmt.Sprintf("c2 only received %d bytes", c2.BytesReceived())
				}
			})

			// now publish on C2
			writers = publishTracksForClients(t, c2)
			defer stopWriters(writers...)

			testutils.WithTimeout(t, func() string {
				if len(c1.SubscribedTracks()[c2.ID()]) == 2 {
					return ""
				} else {
					return fmt.Sprintf("expected c1 to receive 2 tracks from c2, actual: %d", len(c1.SubscribedTracks()[c2.ID()]))
				}
			})

			// now leave, and ensure that it's immediate
			c2.Stop()

			testutils.WithTimeout(t, func() string {
				if len(c1.RemoteParticipants()) > 0 {
					return fmt.Sprintf("expected no remote participants, actual: %v", c1.RemoteParticipants())
				}
				return ""
			})
		})
	}
}

func scenarioDataPublish(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("dp1", defaultServerPort, useSinglePeerConnection, nil)
			c2 := createRTCClient("dp2", secondServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1, c2)
			defer stopClients(c1, c2)

			payload := "test bytes"

			received := atomic.NewBool(false)
			c2.OnDataReceived = func(data []byte, sid string) {
				if string(data) == payload && livekit.ParticipantID(sid) == c1.ID() {
					received.Store(true)
				}
			}

			require.NoError(t, c1.PublishData([]byte(payload), livekit.DataPacket_RELIABLE))

			testutils.WithTimeout(t, func() string {
				if received.Load() {
					return ""
				} else {
					return "c2 did not receive published data"
				}
			})
		})
	}
}

func scenarioDataUnlabeledPublish(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("dp1", defaultServerPort, useSinglePeerConnection, nil)
			c2 := createRTCClient("dp2", secondServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1, c2)
			defer stopClients(c1, c2)

			payload := "test unlabeled bytes"

			received := atomic.NewBool(false)
			c2.OnDataReceived = func(data []byte, _sid string) {
				if string(data) == payload {
					received.Store(true)
				}
			}

			require.NoError(t, c1.PublishDataUnlabeled([]byte(payload)))

			testutils.WithTimeout(t, func() string {
				if received.Load() {
					return ""
				} else {
					return "c2 did not receive published data unlabeled"
				}
			})
		})
	}
}

func scenarioJoinClosedRoom(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			c1 := createRTCClient("jcr1", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1)

			// close room with room client
			_, err := roomClient.DeleteRoom(contextWithToken(createRoomToken()), &livekit.DeleteRoomRequest{
				Room: testRoom,
			})
			require.NoError(t, err)

			// now join again
			c2 := createRTCClient("jcr2", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c2)
			stopClients(c2)
		})
	}
}

// close a room that has been created, but no participant has joined
func closeNonRTCRoom(t *testing.T) {
	createCtx := contextWithToken(createRoomToken())
	_, err := roomClient.CreateRoom(createCtx, &livekit.CreateRoomRequest{
		Name: testRoom,
	})
	require.NoError(t, err)

	_, err = roomClient.DeleteRoom(createCtx, &livekit.DeleteRoomRequest{
		Room: testRoom,
	})
	require.NoError(t, err)
}

func publishTracksForClients(t *testing.T, clients ...*testclient.RTCClient) []*testclient.TrackWriter {
	logger.Infow("publishing tracks for clients")
	var writers []*testclient.TrackWriter
	for i := range clients {
		c := clients[i]
		tw, err := c.AddStaticTrack("audio/opus", "audio", "webcam")
		require.NoError(t, err)

		writers = append(writers, tw)
		tw, err = c.AddStaticTrack("video/vp8", "video", "webcam")
		require.NoError(t, err)
		writers = append(writers, tw)
	}
	return writers
}

// Room service tests

func roomServiceListRoom(t *testing.T) {
	createCtx := contextWithToken(createRoomToken())
	listCtx := contextWithToken(listRoomToken())
	// create rooms
	_, err := roomClient.CreateRoom(createCtx, &livekit.CreateRoomRequest{
		Name: testRoom,
	})
	require.NoError(t, err)
	_, err = roomClient.CreateRoom(contextWithToken(createRoomToken()), &livekit.CreateRoomRequest{
		Name: "yourroom",
	})
	require.NoError(t, err)

	t.Run("list all rooms", func(t *testing.T) {
		res, err := roomClient.ListRooms(listCtx, &livekit.ListRoomsRequest{})
		require.NoError(t, err)
		require.Len(t, res.Rooms, 2)
	})
	t.Run("list specific rooms", func(t *testing.T) {
		res, err := roomClient.ListRooms(listCtx, &livekit.ListRoomsRequest{
			Names: []string{"yourroom"},
		})
		require.NoError(t, err)
		require.Len(t, res.Rooms, 1)
		require.Equal(t, "yourroom", res.Rooms[0].Name)
	})
}
