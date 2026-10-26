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

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/testutils"
	"github.com/livekit/livekit-server/test/client"
)

func TestMultiNodeRouting(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTest("TestMultiNodeRouting")
	defer finish()

	// creating room on node 1
	_, err := roomClient.CreateRoom(contextWithToken(createRoomToken()), &livekit.CreateRoomRequest{
		Name: testRoom,
	})
	require.NoError(t, err)

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			// one node connecting to node 1, and another connecting to node 2
			c1 := createRTCClient("c1", defaultServerPort, testRTCServicePath, nil)
			c2 := createRTCClient("c2", secondServerPort, testRTCServicePath, nil)
			waitUntilConnected(t, c1, c2)
			defer stopClients(c1, c2)

			// c1 publishing, and c2 receiving
			t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
			require.NoError(t, err)
			if t1 != nil {
				defer t1.Stop()
			}

			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()) == 0 {
					return "c2 received no tracks"
				}
				if len(c2.SubscribedTracks()[c1.ID()]) != 1 {
					return "c2 didn't receive track published by c1"
				}
				tr1 := c2.SubscribedTracks()[c1.ID()][0]
				streamID, _ := rtc.UnpackStreamID(tr1.StreamID())
				require.Equal(t, c1.ID(), streamID)
				return ""
			})

			remoteC1 := c2.GetRemoteParticipant(c1.ID())
			require.Equal(t, "c1", remoteC1.Name)
			require.Equal(t, "metadatac1", remoteC1.Metadata)
		})
	}
}

func TestConnectWithoutCreation(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTest("TestConnectWithoutCreation")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			c1 := createRTCClient("c1", defaultServerPort, testRTCServicePath, nil)
			waitUntilConnected(t, c1)

			c1.Stop()
		})
	}
}

func TestMultinodePublishingUponJoining(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, _, finish := setupMultiNodeTest("TestMultinodePublishingUponJoining")
	defer finish()

	scenarioPublishingUponJoining(t)
}

func TestMultinodeReceiveBeforePublish(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	_, _, finish := setupMultiNodeTest("TestMultinodeReceiveBeforePublish")
	defer finish()

	scenarioReceiveBeforePublish(t)
}

// reconnecting to the same room, after one of the servers has gone away
func TestMultinodeReconnectAfterNodeShutdown(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			_, s2, finish := setupMultiNodeTest("TestMultinodeReconnectAfterNodeShutdown")
			defer finish()

			// creating room on node 1
			_, err := roomClient.CreateRoom(contextWithToken(createRoomToken()), &livekit.CreateRoomRequest{
				Name:   testRoom,
				NodeId: s2.Node().Id,
			})
			require.NoError(t, err)

			// one node connecting to node 1, and another connecting to node 2
			c1 := createRTCClient("c1", defaultServerPort, testRTCServicePath, nil)
			c2 := createRTCClient("c2", secondServerPort, testRTCServicePath, nil)

			waitUntilConnected(t, c1, c2)
			stopClients(c1, c2)

			// stop s2, and connect to room again
			s2.Stop(true)

			time.Sleep(syncDelay)

			c3 := createRTCClient("c3", defaultServerPort, testRTCServicePath, nil)
			waitUntilConnected(t, c3)
		})
	}
}

func TestMultinodeDataPublishing(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTest("TestMultinodeDataPublishing")
	defer finish()

	scenarioDataPublish(t)
	scenarioDataUnlabeledPublish(t)
	scenarioDataTracksPublishingUponJoining(t)
}

func TestMultiNodeJoinAfterClose(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTest("TestMultiNodeJoinAfterClose")
	defer finish()

	scenarioJoinClosedRoom(t)
}

func TestMultiNodeCloseNonRTCRoom(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTest("closeNonRTCRoom")
	defer finish()

	closeNonRTCRoom(t)
}

// ensure that token accurately reflects out of band updates
func TestMultiNodeRefreshToken(t *testing.T) {
	_, _, finish := setupMultiNodeTest("TestMultiNodeJoinAfterClose")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			// a participant joining with full permissions
			c1 := createRTCClient("c1", defaultServerPort, testRTCServicePath, nil)
			waitUntilConnected(t, c1)

			// update permissions and metadata
			ctx := contextWithToken(adminRoomToken(testRoom))
			_, err := roomClient.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
				Room:     testRoom,
				Identity: "c1",
				Permission: &livekit.ParticipantPermission{
					CanPublish:   false,
					CanSubscribe: true,
				},
				Metadata: "metadata",
			})
			require.NoError(t, err)

			testutils.WithTimeout(t, func() string {
				if c1.RefreshToken() == "" {
					return "did not receive refresh token"
				}
				// parse token to ensure it's correct
				verifier, err := auth.ParseAPIToken(c1.RefreshToken())
				require.NoError(t, err)

				_, grants, err := verifier.Verify(testApiSecret)
				require.NoError(t, err)

				if grants.Metadata != "metadata" {
					return "metadata did not match"
				}
				if *grants.Video.CanPublish {
					return "canPublish should be false"
				}
				if *grants.Video.CanPublishData {
					return "canPublishData should be false"
				}
				if !*grants.Video.CanSubscribe {
					return "canSubscribe should be true"
				}
				return ""
			})
		})
	}
}

// ensure that token accurately reflects out of band updates
func TestMultiNodeUpdateAttributes(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTest("TestMultiNodeUpdateAttributes")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			c1 := createRTCClient("au1", defaultServerPort, testRTCServicePath, &client.Options{
				TokenCustomizer: func(token *auth.AccessToken, grants *auth.VideoGrant) {
					token.SetAttributes(map[string]string{
						"mykey": "au1",
					})
				},
			})
			c2 := createRTCClient("au2", secondServerPort, testRTCServicePath, &client.Options{
				TokenCustomizer: func(token *auth.AccessToken, grants *auth.VideoGrant) {
					token.SetAttributes(map[string]string{
						"mykey": "au2",
					})
					grants.SetCanUpdateOwnMetadata(true)
				},
			})
			waitUntilConnected(t, c1, c2)

			testutils.WithTimeout(t, func() string {
				rc2 := c1.GetRemoteParticipant(c2.ID())
				rc1 := c2.GetRemoteParticipant(c1.ID())
				if rc2 == nil || rc1 == nil {
					return "participants could not see each other"
				}
				if rc1.Attributes == nil || rc1.Attributes["mykey"] != "au1" {
					return "rc1's initial attributes are incorrect"
				}
				if rc2.Attributes == nil || rc2.Attributes["mykey"] != "au2" {
					return "rc2's initial attributes are incorrect"
				}
				return ""
			})

			// this one should not go through
			_ = c1.SetAttributes(map[string]string{"mykey": "shouldnotchange"})
			_ = c2.SetAttributes(map[string]string{"secondkey": "au2"})

			// updates using room API should succeed
			_, err := roomClient.UpdateParticipant(contextWithToken(adminRoomToken(testRoom)), &livekit.UpdateParticipantRequest{
				Room:     testRoom,
				Identity: "au1",
				Attributes: map[string]string{
					"secondkey": "au1",
				},
			})
			require.NoError(t, err)

			testutils.WithTimeout(t, func() string {
				rc1 := c2.GetRemoteParticipant(c1.ID())
				rc2 := c1.GetRemoteParticipant(c2.ID())
				if rc1.Attributes["secondkey"] != "au1" {
					return "au1's attribute update failed"
				}
				if rc2.Attributes["secondkey"] != "au2" {
					return "au2's attribute update failed"
				}
				if rc1.Attributes["mykey"] != "au1" {
					return "au1's mykey should not change"
				}
				if rc2.Attributes["mykey"] != "au2" {
					return "au2's mykey should not change"
				}
				return ""
			})
		})
	}
}

func TestMultiNodeRevokePublishPermission(t *testing.T) {
	_, _, finish := setupMultiNodeTest("TestMultiNodeRevokePublishPermission")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			c1 := createRTCClient("c1", defaultServerPort, testRTCServicePath, nil)
			c2 := createRTCClient("c2", secondServerPort, testRTCServicePath, nil)
			waitUntilConnected(t, c1, c2)

			// c1 publishes a track for c2
			writers := publishTracksForClients(t, c1)
			defer stopWriters(writers...)

			testutils.WithTimeout(t, func() string {
				if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
					return "c2 did not receive c1's tracks"
				}
				return ""
			})

			// revoke permission
			ctx := contextWithToken(adminRoomToken(testRoom))
			_, err := roomClient.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
				Room:     testRoom,
				Identity: "c1",
				Permission: &livekit.ParticipantPermission{
					CanPublish:     false,
					CanPublishData: true,
					CanSubscribe:   true,
				},
			})
			require.NoError(t, err)

			// ensure c1 no longer has track published, c2 no longer see track under C1
			testutils.WithTimeout(t, func() string {
				if len(c1.GetPublishedTrackIDs()) != 0 {
					return "c1 did not unpublish tracks"
				}
				remoteC1 := c2.GetRemoteParticipant(c1.ID())
				if remoteC1 == nil {
					return "c2 doesn't know about c1"
				}
				if len(remoteC1.Tracks) != 0 {
					return "c2 still has c1's tracks"
				}
				return ""
			})
		})
	}
}

func TestCloseDisconnectedParticipantOnSignalClose(t *testing.T) {
	_, _, finish := setupMultiNodeTest("TestCloseDisconnectedParticipantOnSignalClose")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			c1 := createRTCClient("c1", secondServerPort, testRTCServicePath, nil)
			waitUntilConnected(t, c1)

			c2 := createRTCClient("c2", defaultServerPort, testRTCServicePath, &client.Options{
				SignalRequestInterceptor: func(msg *livekit.SignalRequest, next client.SignalRequestHandler) error {
					switch msg.Message.(type) {
					case *livekit.SignalRequest_Offer, *livekit.SignalRequest_Answer, *livekit.SignalRequest_Leave:
						return nil
					default:
						return next(msg)
					}
				},
				SignalResponseInterceptor: func(msg *livekit.SignalResponse, next client.SignalResponseHandler) error {
					switch msg.Message.(type) {
					case *livekit.SignalResponse_Offer, *livekit.SignalResponse_Answer:
						return nil
					default:
						return next(msg)
					}
				},
			})

			testutils.WithTimeout(t, func() string {
				if len(c1.RemoteParticipants()) != 1 {
					return "c1 did not see c2 join"
				}
				return ""
			})

			c2.Stop()

			testutils.WithTimeout(t, func() string {
				if len(c1.RemoteParticipants()) != 0 {
					return "c1 did not see c2 removed"
				}
				return ""
			})
		})
	}
}

func TestMultiNodeDataBlob(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTestWithConfig("TestMultiNodeDataBlob", func(c *config.Config) {
		c.EnableParticipantDataBlob = true
		c.Limit.MaxDataBlobSize = 1024
	})
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			pubCapture := &dataBlobCapture{}
			subCapture := &dataBlobCapture{}

			// publisher on node 1, subscriber on node 2
			pub := createRTCClient("pub", defaultServerPort, testRTCServicePath, &client.Options{
				AutoSubscribe:             true,
				SignalResponseInterceptor: pubCapture.interceptor(),
			})
			sub := createRTCClient("sub", secondServerPort, testRTCServicePath, &client.Options{
				AutoSubscribe:             true,
				SignalResponseInterceptor: subCapture.interceptor(),
			})
			waitUntilConnected(t, pub, sub)
			defer stopClients(pub, sub)

			// wait for both nodes to see each other so the get request routes correctly
			testutils.WithTimeout(t, func() string {
				if sub.GetRemoteParticipant(pub.ID()) == nil {
					return "sub does not see pub yet"
				}
				return ""
			})

			key := &livekit.DataBlobKey{
				Key: &livekit.DataBlobKey_Generic{
					Generic: "blob-multinode",
				},
			}
			contents := []byte("multinode-content")

			require.NoError(t, pub.SendRequest(&livekit.SignalRequest{
				Message: &livekit.SignalRequest_StoreDataBlobRequest{
					StoreDataBlobRequest: &livekit.StoreDataBlobRequest{
						RequestId: 1,
						Blob: &livekit.DataBlob{
							Key:      key,
							Contents: contents,
						},
					},
				},
			}))

			testutils.WithTimeout(t, func() string {
				resp := pubCapture.takeStoreResponse()
				if resp == nil {
					return "publisher did not receive store response"
				}
				if resp.RequestId != 1 {
					return fmt.Sprintf("expected store response request id 1, got %d", resp.RequestId)
				}
				if resp.Key == nil {
					return "store response missing key"
				}
				if resp.Key.String() != key.String() {
					return fmt.Sprintf("expected stored blob key %s, got %s", key.String(), resp.Key.String())
				}
				return ""
			})
			require.Equal(t, 0, pubCapture.requestResponseCount(), "publisher should not receive an error response on success")

			// subscriber on a different node asks for the blob; the request routes
			// across nodes to the publisher.
			require.NoError(t, sub.SendRequest(&livekit.SignalRequest{
				Message: &livekit.SignalRequest_GetDataBlobRequest{
					GetDataBlobRequest: &livekit.GetDataBlobRequest{
						ParticipantIdentity: "pub",
						Key:                 key,
					},
				},
			}))

			testutils.WithTimeout(t, func() string {
				resp := subCapture.takeBlobResponse()
				if resp == nil {
					return "subscriber did not receive blob response"
				}
				if resp.Blob == nil {
					return "blob response missing blob"
				}
				if resp.Blob.Key.String() != key.String() {
					return fmt.Sprintf("expected data blob key %s, got %s", key.String(), resp.Blob.Key.String())
				}
				if string(resp.Blob.Contents) != string(contents) {
					return fmt.Sprintf("expected contents %q, got %q", contents, resp.Blob.Contents)
				}
				return ""
			})

			// requesting an unknown publisher identity should return NOT_FOUND
			require.NoError(t, sub.SendRequest(&livekit.SignalRequest{
				Message: &livekit.SignalRequest_GetDataBlobRequest{
					GetDataBlobRequest: &livekit.GetDataBlobRequest{
						ParticipantIdentity: "unknown-publisher",
						Key:                 key,
					},
				},
			}))

			testutils.WithTimeout(t, func() string {
				rr := subCapture.takeRequestResponse()
				if rr == nil {
					return "subscriber did not receive RequestResponse for unknown publisher"
				}
				if rr.Reason != livekit.RequestResponse_NOT_FOUND {
					return fmt.Sprintf("expected NOT_FOUND, got %s", rr.Reason)
				}
				return ""
			})
		})
	}
}
