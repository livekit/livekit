package test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/cmd/cli/client"
	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
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

func setupSingleNodeTest(roomName string) *service.LivekitServer {
	logger.InitDevelopment("")
	s := createSingleNodeServer()
	go func() {
		s.Start()
	}()

	waitForServerToStart(s)

	// create test room
	header := make(http.Header)
	client.SetAuthorizationToken(header, createRoomToken())
	tctx, err := twirp.WithHTTPRequestHeaders(context.Background(), header)
	if err != nil {
		panic(err)
	}
	_, err = roomClient.CreateRoom(tctx, &livekit.CreateRoomRequest{Name: roomName})
	if err != nil {
		panic(err)
	}
	return s
}

func setupMultiNodeTest(roomName string) *service.LivekitServer {
	logger.InitDevelopment("")
	s := createMultiNodeServer()
	go func() {
		s.Start()
	}()

	waitForServerToStart(s)

	// create test room
	header := make(http.Header)
	client.SetAuthorizationToken(header, createRoomToken())
	tctx, err := twirp.WithHTTPRequestHeaders(context.Background(), header)
	if err != nil {
		panic(err)
	}
	_, err = roomClient.CreateRoom(tctx, &livekit.CreateRoomRequest{Name: roomName})
	if err != nil {
		panic(err)
	}
	return s
}

func teardownTest(s *service.LivekitServer, roomName string) {
	header := make(http.Header)
	client.SetAuthorizationToken(header, createRoomToken())
	tctx, err := twirp.WithHTTPRequestHeaders(context.Background(), header)
	if err != nil {
		panic(err)
	}
	roomClient.DeleteRoom(tctx, &livekit.DeleteRoomRequest{Room: roomName})
	s.Stop()
}

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

func withTimeout(t *testing.T, description string, f func() bool) {
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out: " + description)
		case <-time.After(10 * time.Millisecond):
			if f() {
				return
			}
		}
	}
}

func waitUntilConnected(t *testing.T, clients ...*client.RTCClient) {
	wg := sync.WaitGroup{}
	for i := range clients {
		c := clients[i]
		wg.Add(1)
		go func() {
			assert.NoError(t, c.WaitUntilConnected())
			wg.Done()
		}()
	}
	wg.Wait()
}

func createSingleNodeServer() *service.LivekitServer {
	var err error
	serverConfig, err = config.NewConfig("")
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}

	currentNode, err := routing.NewLocalNode(serverConfig)
	if err != nil {
		panic(err)
	}

	// local routing and store
	router := routing.NewLocalRouter(currentNode)
	roomStore := service.NewLocalRoomStore()
	s, err := service.InitializeServer(serverConfig, &StaticKeyProvider{}, roomStore, router, currentNode)
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", serverConfig.Port), &http.Client{})
	return s
}

func createMultiNodeServer() *service.LivekitServer {
	var err error
	serverConfig, err = config.NewConfig("")
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	serverConfig.MultiNode = true
	serverConfig.Redis.Address = "localhost:6379"

	currentNode, err := routing.NewLocalNode(serverConfig)
	if err != nil {
		panic(err)
	}

	// local routing and store
	rc := redis.NewClient(&redis.Options{
		Addr:     serverConfig.Redis.Address,
		Password: serverConfig.Redis.Password,
	})
	if err = rc.Ping(context.Background()).Err(); err != nil {
		panic(err)
	}

	router := routing.NewRedisRouter(currentNode, rc, false)
	roomStore := service.NewRedisRoomStore(rc)
	s, err := service.InitializeServer(serverConfig, &StaticKeyProvider{}, roomStore, router, currentNode)
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", serverConfig.Port), &http.Client{})
	return s
}

// creates a client and runs against server
func createRTCClient(name string) *client.RTCClient {
	token := joinToken(testRoom, name)
	ws, err := client.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", serverConfig.Port), token)
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
		return testApiSecret
	}
	return ""
}
