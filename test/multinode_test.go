package test

import (
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/testutils"
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

	testutils.WithTimeout(t, "c2 should receive one track", func() bool {
		if len(c2.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c2.SubscribedTracks()[c1.ID()]) != 1 {
			return false
		}

		tr1 := c2.SubscribedTracks()[c1.ID()][0]
		streamID, _ := rtc.UnpackStreamID(tr1.StreamID())
		require.Equal(t, c1.ID(), streamID)
		return true
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
