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
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
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

// a node killed without shutting down leaves its rooms assigned to it
func TestMultiNodeDeleteRoomOfNodeThatLeft(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	for name, leaveCluster := range map[string]func(t *testing.T, nodeID string){
		"node unregistered": func(t *testing.T, nodeID string) {},
		"node stopped updating": func(t *testing.T, nodeID string) {
			data, err := proto.Marshal(&livekit.Node{
				Id:    nodeID,
				State: livekit.NodeState_SERVING,
				Stats: &livekit.NodeStats{UpdatedAt: time.Now().Add(-time.Hour).Unix()},
			})
			require.NoError(t, err)
			require.NoError(t, redisClient().HSet(context.Background(), routing.NodesKey, nodeID, data).Err())
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeDeleteRoomOfNodeThatLeft")
			defer finish()

			// the room as an unclean shutdown leaves it: stored and assigned to a node that is gone
			now := time.Now()
			require.NoError(t, service.NewRedisStore(redisClient()).StoreRoom(context.Background(), &livekit.Room{
				Sid:            "RM_orphaned",
				Name:           "orphanedRoom",
				CreationTime:   now.Unix(),
				CreationTimeMs: now.UnixMilli(),
				EmptyTimeout:   300,
			}, nil))
			leaveCluster(t, "ND_gone")
			require.NoError(t, redisClient().HSet(context.Background(), routing.NodeRoomKey, "orphanedRoom", "ND_gone").Err())

			_, err := roomClient.DeleteRoom(contextWithToken(createRoomToken()), &livekit.DeleteRoomRequest{
				Room: "orphanedRoom",
			})
			require.NoError(t, err)

			testutils.WithTimeout(t, func() string {
				res, err := roomClient.ListRooms(contextWithToken(listRoomToken()), &livekit.ListRoomsRequest{})
				if err != nil {
					return err.Error()
				}
				if len(res.Rooms) != 0 {
					return fmt.Sprintf("expected no rooms, got %d", len(res.Rooms))
				}
				return ""
			})
		})
	}
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
		for _, testRTCServicePath := range testRTCServicePaths {
			t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
				_, _, finish := setupMultiNodeTest("TestMultiNodeUpdateRoomMetadata_with_participant")
				defer finish()

				c1 := createRTCClient("c1", defaultServerPort, testRTCServicePath, nil)
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

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeRemoveParticipant")
			defer finish()

			c1 := createRTCClient("mn_remove_participant", defaultServerPort, testRTCServicePath, nil)
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
	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeUpdateParticipantMetadata")
			defer finish()

			c1 := createRTCClient("update_participant_metadata", defaultServerPort, testRTCServicePath, nil)
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
	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeMutePublishedTrack")
			defer finish()

			identity := "mute_published_track"
			c1 := createRTCClient(identity, defaultServerPort, testRTCServicePath, nil)
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
