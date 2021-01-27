package test

import (
	"context"
	"testing"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/proto/livekit"
)

func TestMultiNodeRouting(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	s1, s2 := setupMultiNodeTest()
	defer s1.Stop()
	defer s2.Stop()

	// creating room on node 1
	_, err := roomClient.CreateRoom(contextWithCreateRoomToken(), &livekit.CreateRoomRequest{
		Name:   testRoom,
		NodeId: nodeId1,
	})
	assert.NoError(t, err)

	// one node connecting to node 1, and another connecting to node 2
	c1 := createRTCClient("c1", defaultServerPort)
	c2 := createRTCClient("c2", secondServerPort)
	waitUntilConnected(t, c1, c2)

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
		assert.Equal(t, "webcam", tr1.StreamID())
		return true
	})

	c1.Stop()
	c2.Stop()

	// ensure that room is closed
	rc := redisClient()
	ctx := context.Background()
	withTimeout(t, "room should be closed", func() bool {
		if rc.HGet(ctx, service.RoomsKey, testRoom).Err() == nil {
			return false
		}
		return true
	})

	assert.Equal(t, redis.Nil, rc.HGet(ctx, routing.NodeRoomKey, testRoom).Err())
	assert.Equal(t, redis.Nil, rc.HGet(ctx, service.RoomIdMap, testRoom).Err())
}

func TestConnectWithoutCreation(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	s1, s2 := setupMultiNodeTest()
	defer s1.Stop()
	defer s2.Stop()

	c1 := createRTCClient("c1", defaultServerPort)
	waitUntilConnected(t, c1)

	c1.Stop()
}
