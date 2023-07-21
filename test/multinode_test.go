package test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

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

	// one node connecting to node 1, and another connecting to node 2
	c1 := createRTCClient("c1", defaultServerPort, nil)
	c2 := createRTCClient("c2", secondServerPort, nil)
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
}

func TestConnectWithoutCreation(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTest("TestConnectWithoutCreation")
	defer finish()

	c1 := createRTCClient("c1", defaultServerPort, nil)
	waitUntilConnected(t, c1)

	c1.Stop()
}

// testing multiple scenarios  rooms
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

	_, s2, finish := setupMultiNodeTest("TestMultinodeReconnectAfterNodeShutdown")
	defer finish()

	// creating room on node 1
	_, err := roomClient.CreateRoom(contextWithToken(createRoomToken()), &livekit.CreateRoomRequest{
		Name:   testRoom,
		NodeId: s2.Node().Id,
	})
	require.NoError(t, err)

	// one node connecting to node 1, and another connecting to node 2
	c1 := createRTCClient("c1", defaultServerPort, nil)
	c2 := createRTCClient("c2", secondServerPort, nil)

	waitUntilConnected(t, c1, c2)
	stopClients(c1, c2)

	// stop s2, and connect to room again
	s2.Stop(true)

	time.Sleep(syncDelay)

	c3 := createRTCClient("c3", defaultServerPort, nil)
	waitUntilConnected(t, c3)
}

func TestMultinodeDataPublishing(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, _, finish := setupMultiNodeTest("TestMultinodeDataPublishing")
	defer finish()

	scenarioDataPublish(t)
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

	// a participant joining with full permissions
	c1 := createRTCClient("c1", defaultServerPort, nil)
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

		grants, err := verifier.Verify(testApiSecret)
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
}

func TestMultiNodeRevokePublishPermission(t *testing.T) {
	_, _, finish := setupMultiNodeTest("TestMultiNodeRevokePublishPermission")
	defer finish()

	c1 := createRTCClient("c1", defaultServerPort, nil)
	c2 := createRTCClient("c2", secondServerPort, nil)
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
}

func TestCloseDisconnectedParticipantOnSignalClose(t *testing.T) {
	_, _, finish := setupMultiNodeTest("TestCloseDisconnectedParticipantOnSignalClose")
	defer finish()

	c1 := createRTCClient("c1", secondServerPort, nil)
	waitUntilConnected(t, c1)

	c2 := createRTCClient("c2", defaultServerPort, &client.Options{
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
}
