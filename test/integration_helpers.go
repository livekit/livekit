package test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
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
	testApiKey        = "apikey"
	testApiSecret     = "apiSecret"
	testRoom          = "mytestroom"
	defaultServerPort = 7880
	secondServerPort  = 7881
	nodeId1           = "node-1"
	nodeId2           = "node-2"

	syncDelay      = 100 * time.Millisecond
	connectTimeout = 10 * time.Second
	// if there are deadlocks, it's helpful to set a short test timeout (i.e. go test -timeout=30s)
	// let connection timeout happen
	//connectTimeout = 5000 * time.Second
)

var (
	roomClient livekit.RoomService
)

func init() {
	logger.InitDevelopment("")
}

func setupSingleNodeTest(roomName string) *service.LivekitServer {
	s := createSingleNodeServer()
	go func() {
		s.Start()
	}()

	waitForServerToStart(s)

	// create test room
	_, err := roomClient.CreateRoom(contextWithCreateRoomToken(), &livekit.CreateRoomRequest{Name: roomName})
	if err != nil {
		panic(err)
	}
	return s
}

func setupMultiNodeTest() (*service.LivekitServer, *service.LivekitServer) {
	s1 := createMultiNodeServer(nodeId1, defaultServerPort)
	s2 := createMultiNodeServer(nodeId2, secondServerPort)
	go s1.Start()
	go s2.Start()

	waitForServerToStart(s1)
	waitForServerToStart(s2)

	return s1, s2
}

func teardownTest(s *service.LivekitServer, roomName string) {
	roomClient.DeleteRoom(contextWithCreateRoomToken(), &livekit.DeleteRoomRequest{Room: roomName})
	s.Stop()
}

func contextWithCreateRoomToken() context.Context {
	header := make(http.Header)
	client.SetAuthorizationToken(header, createRoomToken())
	tctx, err := twirp.WithHTTPRequestHeaders(context.Background(), header)
	if err != nil {
		panic(err)
	}
	return tctx
}

func waitForServerToStart(s *service.LivekitServer) {
	// wait till ready
	ctx, _ := context.WithTimeout(context.Background(), connectTimeout)
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

func withTimeout(t *testing.T, description string, f func() bool) bool {
	logger.Infow(description)
	ctx, _ := context.WithTimeout(context.Background(), connectTimeout)
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out: " + description)
			return false
		case <-time.After(10 * time.Millisecond):
			if f() {
				return true
			}
		}
	}
}

func waitUntilConnected(t *testing.T, clients ...*client.RTCClient) {
	logger.Infow("waiting for clients to become connected")
	wg := sync.WaitGroup{}
	errChan := make(chan error, len(clients))
	for i := range clients {
		c := clients[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.WaitUntilConnected()
			if err != nil {
				errChan <- err
			}
		}()
	}
	wg.Wait()
	close(errChan)
	hasError := false
	for err := range errChan {
		t.Fatal(err)
		hasError = true
	}
	if hasError {
		t.FailNow()
	}
}

func createSingleNodeServer() *service.LivekitServer {
	var err error
	conf, err := config.NewConfig("")
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}

	currentNode, err := routing.NewLocalNode(conf)
	currentNode.Id = nodeId1
	if err != nil {
		panic(err)
	}

	// local routing and store
	router := routing.NewLocalRouter(currentNode)
	roomStore := service.NewLocalRoomStore()
	s, err := service.InitializeServer(conf, &StaticKeyProvider{}, roomStore, router, currentNode, &routing.RandomSelector{})
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", defaultServerPort), &http.Client{})
	return s
}

func createMultiNodeServer(nodeId string, port uint32) *service.LivekitServer {
	var err error
	conf, err := config.NewConfig("")
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.Port = port
	conf.Redis.Address = "localhost:6379"

	currentNode, err := routing.NewLocalNode(conf)
	currentNode.Id = nodeId
	if err != nil {
		panic(err)
	}

	// local routing and store
	rc := redisClient()
	if err = rc.Ping(context.Background()).Err(); err != nil {
		panic(err)
	}

	router := routing.NewRedisRouter(currentNode, rc)
	roomStore := service.NewRedisRoomStore(rc)
	s, err := service.InitializeServer(conf, &StaticKeyProvider{}, roomStore, router, currentNode, &routing.RandomSelector{})
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", port), &http.Client{})
	return s
}

// creates a client and runs against server
func createRTCClient(name string, port int) *client.RTCClient {
	token := joinToken(testRoom, name)
	ws, err := client.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", port), token)
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

func redisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
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

func stopWriters(writers ...*client.TrackWriter) {
	for _, w := range writers {
		w.Stop()
	}
}

func stopClients(clients ...*client.RTCClient) {
	for _, c := range clients {
		c.Stop()
	}
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
