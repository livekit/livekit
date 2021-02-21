package test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/proto/livekit"
)

func TestMultiNodeRouting(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	logger.Infow("\n\n---Starting TestMultiNodeRouting---")
	defer logger.Infow("---Finishing TestMultiNodeRouting---")

	s1, s2 := setupMultiNodeTest()
	defer s1.Stop()
	defer s2.Stop()

	// creating room on node 1
	_, err := roomClient.CreateRoom(contextWithCreateRoomToken(), &livekit.CreateRoomRequest{
		Name: testRoom,
	})
	assert.NoError(t, err)

	// one node connecting to node 1, and another connecting to node 2
	c1 := createRTCClient("c1", defaultServerPort)
	c2 := createRTCClient("c2", secondServerPort)
	waitUntilConnected(t, c1, c2)
	defer stopClients(c1, c2)

	// c1 publishing, and c2 receiving
	t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
	assert.NoError(t, err)
	if t1 != nil {
		defer t1.Stop()
	}

	withTimeout(t, "c2 should receive one track", func() bool {
		if len(c2.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c2.SubscribedTracks()[c1.ID()]) != 1 {
			return false
		}

		tr1 := c2.SubscribedTracks()[c1.ID()][0]
		assert.Equal(t, c1.ID(), tr1.StreamID())
		return true
	})
}

func TestConnectWithoutCreation(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	logger.Infow("\n\n---Starting TestConnectWithoutCreation---")
	defer logger.Infow("---Finishing TestConnectWithoutCreation---")

	s1, s2 := setupMultiNodeTest()
	defer s1.Stop()
	defer s2.Stop()

	c1 := createRTCClient("c1", defaultServerPort)
	waitUntilConnected(t, c1)

	c1.Stop()
}

// testing multiple scenarios  rooms
func TestMultinodePublishingUponJoining(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	logger.Infow("\n\n---Starting TestMultinodePublishingUponJoining---")
	defer logger.Infow("---Finishing TestMultinodePublishingUponJoining---")

	s1, s2 := setupMultiNodeTest()
	defer s1.Stop()
	defer s2.Stop()

	scenarioPublishingUponJoining(t, defaultServerPort, secondServerPort)
}

func TestMultinodeReceiveBeforePublish(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	logger.Infow("\n\n---Starting TestMultinodeReceiveBeforePublish---")
	defer logger.Infow("---Finishing TestMultinodeReceiveBeforePublish---")

	s1, s2 := setupMultiNodeTest()
	defer s1.Stop()
	defer s2.Stop()

	scenarioReceiveBeforePublish(t)
}

// reconnecting to the same room, after one of the servers has gone away
func TestMultinodeReconnectAfterNodeShutdown(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	logger.Infow("\n\n---Starting TestMultiNodeRouting---")
	defer logger.Infow("---Finishing TestMultiNodeRouting---")

	s1, s2 := setupMultiNodeTest()
	defer s1.Stop()
	defer s2.Stop()

	// creating room on node 1
	_, err := roomClient.CreateRoom(contextWithCreateRoomToken(), &livekit.CreateRoomRequest{
		Name:   testRoom,
		NodeId: s2.Node().Id,
	})
	assert.NoError(t, err)

	// one node connecting to node 1, and another connecting to node 2
	c1 := createRTCClient("c1", defaultServerPort)
	c2 := createRTCClient("c2", secondServerPort)

	waitUntilConnected(t, c1, c2)
	stopClients(c1, c2)

	// stop s2, and connect to room again
	s2.Stop()

	time.Sleep(syncDelay)

	c3 := createRTCClient("c3", defaultServerPort)
	waitUntilConnected(t, c3)
}
