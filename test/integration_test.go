package test

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/proto/livekit"
)

func TestClientCouldConnect(t *testing.T) {
	c1 := createClient("c1")
	c2 := createClient("c2")
	waitUntilConnected(t, c1, c2)

	// ensure they both see each other
	withTimeout(t, func() bool {
		if len(c1.RemoteParticipants()) == 0 || len(c2.RemoteParticipants()) == 0 {
			return false
		}
		//assert.Equal()
		return true
	})
}

func TestSinglePublisher(t *testing.T) {
	c1 := createClient("c1")
	c2 := createClient("c2")
	waitUntilConnected(t, c1, c2)

	// publish a track and ensure clients receive it ok
	t1, err := c1.AddStaticTrack("audio/opus", "audio", "webcam")
	assert.NoError(t, err)
	defer t1.Stop()
	t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
	assert.NoError(t, err)
	defer t2.Stop()

	// a new client joins and should get the initial stream
	c3 := createClient("c3")

	withTimeout(t, func() bool {
		if len(c2.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c2.SubscribedTracks()[c1.ID()]) != 2 {
			return false
		}

		tr1 := c2.SubscribedTracks()[c1.ID()][0]
		assert.Equal(t, "webcam", tr1.StreamID())
		return true
	})

	// ensure that new client that has joined also received tracks
	waitUntilConnected(t, c3)
	withTimeout(t, func() bool {
		if len(c3.SubscribedTracks()) == 0 {
			return false
		}
		// should have received two tracks
		if len(c3.SubscribedTracks()[c1.ID()]) != 2 {
			return false
		}
		return true
	})
}

func TestMain(m *testing.M) {
	logger.InitDevelopment()
	s := createServer()
	service.AuthRequired = true
	go func() {
		s.Start()
	}()

	waitForServerToStart(s)

	// create test room
	token := createRoomToken()
	header := make(http.Header)
	logger.GetLogger().Debugw("auth token", "token", token)
	header.Set("Authorization", "Bearer "+token)
	tctx, err := twirp.WithHTTPRequestHeaders(context.Background(), header)
	if err != nil {
		panic(err)
	}
	_, err = roomClient.CreateRoom(tctx, &livekit.CreateRoomRequest{Name: testRoom})
	if err != nil {
		panic(err)
	}

	code := m.Run()

	roomClient.DeleteRoom(tctx, &livekit.DeleteRoomRequest{Room: testRoom})
	s.Stop()
	os.Exit(code)
}
