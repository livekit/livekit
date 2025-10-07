// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twitchtv/twirp"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
)

const (
	testApiKey        = "apikey"
	testApiSecret     = "apiSecretExtendTo32BytesAsThatIsMinimum"
	testRoom          = "mytestroom"
	defaultServerPort = 7880
	secondServerPort  = 8880
	nodeID1           = "node-1"
	nodeID2           = "node-2"

	syncDelay = 100 * time.Millisecond
	// if there are deadlocks, it's helpful to set a short test timeout (i.e. go test -timeout=30s)
	// let connection timeout happen
	// connectTimeout = 5000 * time.Second
)

var roomClient livekit.RoomService

func init() {
	config.InitLoggerFromConfig(&config.DefaultConfig.Logging)

	prometheus.Init("test", livekit.NodeType_SERVER)
}

func setupSingleNodeTest(name string) (*service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s := createSingleNodeServer(nil)
	go func() {
		if err := s.Start(); err != nil {
			logger.Errorw("server returned error", err)
		}
	}()

	waitForServerToStart(s)

	return s, func() {
		s.Stop(true)
		logger.Infow("----------------FINISHING TEST----------------", "test", name)
	}
}

func setupMultiNodeTest(name string) (*service.LivekitServer, *service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s1 := createMultiNodeServer(guid.New(nodeID1), defaultServerPort)
	s2 := createMultiNodeServer(guid.New(nodeID2), secondServerPort)
	go s1.Start()
	go s2.Start()

	waitForServerToStart(s1)
	waitForServerToStart(s2)

	return s1, s2, func() {
		s1.Stop(true)
		s2.Stop(true)
		redisClient().FlushAll(context.Background())
		logger.Infow("----------------FINISHING TEST----------------", "test", name)
	}
}

func contextWithToken(token string) context.Context {
	header := make(http.Header)
	testclient.SetAuthorizationToken(header, token)
	tctx, err := twirp.WithHTTPRequestHeaders(context.Background(), header)
	if err != nil {
		panic(err)
	}
	return tctx
}

func waitForServerToStart(s *service.LivekitServer) {
	// wait till ready
	ctx, cancel := context.WithTimeout(context.Background(), testutils.ConnectTimeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			panic("could not start server after timeout")
		case <-time.After(10 * time.Millisecond):
			if s.IsRunning() {
				// ensure we can connect to it
				res, err := http.Get(fmt.Sprintf("http://localhost:%d", s.HTTPPort()))
				if err == nil && res.StatusCode == http.StatusOK {
					return
				}
			}
		}
	}
}

func waitUntilConnected(t *testing.T, clients ...*testclient.RTCClient) {
	logger.Infow("waiting for clients to become connected")
	wg := sync.WaitGroup{}
	for i := range clients {
		c := clients[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.WaitUntilConnected()
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

func createSingleNodeServer(configUpdater func(*config.Config)) *service.LivekitServer {
	var err error
	conf, err := config.NewConfig("", true, nil, nil)
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.Keys = map[string]string{testApiKey: testApiSecret}
	if configUpdater != nil {
		configUpdater(conf)
	}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		panic(fmt.Sprintf("could not create local node: %v", err))
	}
	currentNode.SetNodeID(livekit.NodeID(guid.New(nodeID1)))

	s, err := service.InitializeServer(conf, currentNode)
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", defaultServerPort), &http.Client{})
	return s
}

func createMultiNodeServer(nodeID string, port uint32) *service.LivekitServer {
	var err error
	conf, err := config.NewConfig("", true, nil, nil)
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.Port = port
	conf.RTC.UDPPort = rtcconfig.PortRange{Start: int(port) + 1}
	conf.RTC.TCPPort = port + 2
	conf.Redis.Address = "localhost:6379"
	conf.Keys = map[string]string{testApiKey: testApiSecret}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		panic(err)
	}
	currentNode.SetNodeID(livekit.NodeID(nodeID))

	// redis routing and store
	s, err := service.InitializeServer(conf, currentNode)
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", port), &http.Client{})
	return s
}

// creates a client and runs against server
func createRTCClient(name string, port int, useSinglePeerConnection bool, opts *testclient.Options) *testclient.RTCClient {
	var customizer func(token *auth.AccessToken, grants *auth.VideoGrant)
	if opts != nil {
		customizer = opts.TokenCustomizer
	}
	token := joinToken(testRoom, name, customizer)

	return createRTCClientWithToken(token, port, useSinglePeerConnection, opts)
}

// creates a client and runs against server
func createRTCClientWithToken(token string, port int, useSinglePeerConnection bool, opts *testclient.Options) *testclient.RTCClient {
	if opts == nil {
		opts = &testclient.Options{
			AutoSubscribe: true,
		}
	}
	if useSinglePeerConnection {
		opts.UseJoinRequestQueryParam = true
	}
	ws, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", port), token, opts)
	if err != nil {
		panic(err)
	}

	c, err := testclient.NewRTCClient(ws, useSinglePeerConnection, opts)
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

func joinToken(room, name string, customFn func(token *auth.AccessToken, grants *auth.VideoGrant)) string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		SetIdentity(name).
		SetName(name).
		SetMetadata("metadata" + name)
	grant := &auth.VideoGrant{RoomJoin: true, Room: room}
	if customFn != nil {
		customFn(at, grant)
	}
	at.AddGrant(grant)
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

func joinTokenWithGrant(name string, grant *auth.VideoGrant) string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(grant).
		SetIdentity(name).
		SetName(name)
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

func createRoomToken() string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{RoomCreate: true})
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

func adminRoomToken(name string) string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{RoomAdmin: true, Room: name})
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

func listRoomToken() string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{RoomList: true})
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

func stopWriters(writers ...*testclient.TrackWriter) {
	for _, w := range writers {
		w.Stop()
	}
}

func stopClients(clients ...*testclient.RTCClient) {
	for _, c := range clients {
		c.Stop()
	}
}
