package test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/livekit/livekit-server/cmd/cli/client"
	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	testApiKey    = "apikey"
	testApiSecret = "apiSecret"
	testRoom      = "mytestroom"
)

var (
	serverConfig *config.Config
	roomClient   livekit.RoomService
)

func waitForServerToStart(s *service.LivekitServer) {
	// wait till ready
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	for {
		select {
		case <-ctx.Done():
			panic("could not start server after timeout")
		case <-time.After(10 * time.Millisecond):
			if s.IsRunning() {
				return
			}
		}
	}
}

func withTimeout(t *testing.T, f func() bool) {
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out")
		case <-time.After(10 * time.Millisecond):
			if f() {
				return
			}
		}
	}
}

func createServer() *service.LivekitServer {
	var err error
	serverConfig, err = config.NewConfig("")
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	s, err := service.InitializeServer(serverConfig, &StaticKeyProvider{})
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", serverConfig.APIPort), &http.Client{})
	return s
}

// creates a client and runs against server
func createClient(name string) *client.RTCClient {
	token := joinToken(testRoom, name)
	ws, err := client.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", serverConfig.RTCPort), token)
	if err != nil {
		panic(err)
	}

	c, err := client.NewRTCClient(ws)
	if err != nil {
		panic(err)
	}

	go c.Run()

	return c
}

func joinToken(room, name string) string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{RoomJoin: true, Room: room}).
		SetIdentity(name)
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

func createRoomToken() string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{RoomCreate: true}).
		SetIdentity("testuser")
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

type StaticKeyProvider struct {
}

func (p *StaticKeyProvider) NumKeys() int {
	return 1
}

func (p *StaticKeyProvider) GetSecret(key string) string {
	if key == testApiKey {
		logger.GetLogger().Debugf("returning secret: %s", testApiSecret)
		return testApiSecret
	}
	return ""
}
