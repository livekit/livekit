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
	assert.NoError(t, c1.WaitUntilConnected())
	c2 := createClient("c2")
	assert.NoError(t, c2.WaitUntilConnected())

	// ensure they both see each other
	withTimeout(t, func() bool {
		return len(c1.RemoteParticipants()) == 1 && len(c2.RemoteParticipants()) == 1
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
