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

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/testutils"
)

func TestMultiNodeRoomList(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, _, finish := setupMultiNodeTest("TestMultiNodeRoomList")
	defer finish()

	roomServiceListRoom(t)
}

// update room metadata when it's empty
func TestMultiNodeUpdateRoomMetadata(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	t.Run("when room is empty", func(t *testing.T) {
		_, _, finish := setupMultiNodeTest("TestMultiNodeUpdateRoomMetadata_empty")
		defer finish()

		_, err := roomClient.CreateRoom(contextWithToken(createRoomToken()), &livekit.CreateRoomRequest{
			Name: "emptyRoom",
		})
		require.NoError(t, err)

		rm, err := roomClient.UpdateRoomMetadata(contextWithToken(adminRoomToken("emptyRoom")), &livekit.UpdateRoomMetadataRequest{
			Room:     "emptyRoom",
			Metadata: "updated metadata",
		})
		require.NoError(t, err)
		require.Equal(t, "updated metadata", rm.Metadata)
	})

	t.Run("when room has a participant", func(t *testing.T) {
		for _, useSinglePeerConnection := range []bool{false, true} {
			t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
				_, _, finish := setupMultiNodeTest("TestMultiNodeUpdateRoomMetadata_with_participant")
				defer finish()

				c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, nil)
				waitUntilConnected(t, c1)
				defer c1.Stop()

				_, err := roomClient.CreateRoom(contextWithToken(createRoomToken()), &livekit.CreateRoomRequest{
					Name: "emptyRoom",
				})
				require.NoError(t, err)

				rm, err := roomClient.UpdateRoomMetadata(contextWithToken(adminRoomToken("emptyRoom")), &livekit.UpdateRoomMetadataRequest{
					Room:     "emptyRoom",
					Metadata: "updated metadata",
				})
				require.NoError(t, err)
				require.Equal(t, "updated metadata", rm.Metadata)
			})
		}
	})
}

// remove a participant
func TestMultiNodeRemoveParticipant(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeRemoveParticipant")
			defer finish()

			c1 := createRTCClient("mn_remove_participant", defaultServerPort, useSinglePeerConnection, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			ctx := contextWithToken(adminRoomToken(testRoom))
			_, err := roomClient.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
				Room:     testRoom,
				Identity: "mn_remove_participant",
			})
			require.NoError(t, err)

			// participant list doesn't show the participant
			listRes, err := roomClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{
				Room: testRoom,
			})
			require.NoError(t, err)
			require.Len(t, listRes.Participants, 0)
		})
	}
}

// update participant metadata
func TestMultiNodeUpdateParticipantMetadata(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeUpdateParticipantMetadata")
			defer finish()

			c1 := createRTCClient("update_participant_metadata", defaultServerPort, useSinglePeerConnection, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			ctx := contextWithToken(adminRoomToken(testRoom))
			res, err := roomClient.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
				Room:     testRoom,
				Identity: "update_participant_metadata",
				Metadata: "the new metadata",
			})
			require.NoError(t, err)
			require.Equal(t, "the new metadata", res.Metadata)
		})
	}
}

// admin mute published track
func TestMultiNodeMutePublishedTrack(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeMutePublishedTrack")
			defer finish()

			identity := "mute_published_track"
			c1 := createRTCClient(identity, defaultServerPort, useSinglePeerConnection, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			writers := publishTracksForClients(t, c1)
			defer stopWriters(writers...)

			trackIDs := c1.GetPublishedTrackIDs()
			require.NotEmpty(t, trackIDs)

			ctx := contextWithToken(adminRoomToken(testRoom))
			// wait for it to be published before
			testutils.WithTimeout(t, func() string {
				res, err := roomClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
					Room:     testRoom,
					Identity: identity,
				})
				require.NoError(t, err)
				if len(res.Tracks) == 2 {
					return ""
				} else {
					return fmt.Sprintf("expected 2 tracks to be published, actual: %d", len(res.Tracks))
				}
			})

			res, err := roomClient.MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
				Room:     testRoom,
				Identity: identity,
				TrackSid: trackIDs[0],
				Muted:    true,
			})
			require.NoError(t, err)
			require.Equal(t, trackIDs[0], res.Track.Sid)
			require.True(t, res.Track.Muted)
		})
	}
}
