package test

import (
	"context"
	"fmt"
	"time"

	"github.com/livekit/livekit-server/cmd/cli/client"
	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
)

const (
	testApiKey    = "apikey"
	testApiSecret = "apiSecret"
)

func waitForServerToStart(s *service.LivekitServer) {
	// wait till ready
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
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

func createServer() *service.LivekitServer {
	conf, err := config.NewConfig("")
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	s, err := service.InitializeServer(conf, &StaticKeyProvider{})
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}
	return s
}

func createClient(room, name string) *client.RTCClient {
	return nil
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
		return testApiSecret
	}
	return ""
}
