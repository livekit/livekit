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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/transport/v4/vnet"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/pkg/testutils"
	"github.com/livekit/livekit-server/pkg/testutils/vnettest"
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
	return setupSingleNodeTestWithConfig(name, nil)
}

func setupSingleNodeTestWithConfig(name string, configUpdater func(*config.Config)) (*service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s := createSingleNodeServer(configUpdater)
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
	return setupMultiNodeTestWithConfig(name, nil)
}

func setupMultiNodeTestWithConfig(name string, configUpdater func(*config.Config)) (*service.LivekitServer, *service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s1 := createMultiNodeServer(guid.New(nodeID1), defaultServerPort, configUpdater)
	s2 := createMultiNodeServer(guid.New(nodeID2), secondServerPort, configUpdater)
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
		wg.Go(func() {
			err := c.WaitUntilConnected(5 * time.Second)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

func ensureNotConnected(t *testing.T, clients ...*testclient.RTCClient) {
	logger.Infow("checking if clients connect")
	wg := sync.WaitGroup{}
	for i := range clients {
		c := clients[i]
		wg.Go(func() {
			err := c.WaitUntilConnected(5 * time.Second)
			if err == nil {
				t.Error(fmt.Errorf("expected client to not connect: %s", c.ID()))
			}
		})
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
	conf.EnableDataTracks = true
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

func createMultiNodeServer(nodeID string, port uint32, configUpdater func(*config.Config)) *service.LivekitServer {
	var err error
	conf, err := config.NewConfig("", true, nil, nil)
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.Port = port
	conf.RTC.UDPPort = rtcconfig.PortRange{Start: int(port) + 1}
	conf.RTC.TCPPort = port + 2
	conf.Redis.Address = redisAddr
	conf.Keys = map[string]string{testApiKey: testApiSecret}
	conf.EnableDataTracks = true
	if configUpdater != nil {
		configUpdater(conf)
	}

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

type testRTCServicePath int

const (
	testRTCServicePathv0 testRTCServicePath = iota
	testRTCServicePathv0SinglePeerConnection
	testRTCServicePathv1
)

func (t testRTCServicePath) String() string {
	switch t {
	case testRTCServicePathv0:
		return "v0"
	case testRTCServicePathv0SinglePeerConnection:
		return "v0-single-peer-connection"
	case testRTCServicePathv1:
		return "v1"
	default:
		return fmt.Sprintf("unknown: %d", t)
	}
}

var testRTCServicePaths = []testRTCServicePath{
	testRTCServicePathv0,
	testRTCServicePathv0SinglePeerConnection,
	testRTCServicePathv1,
}

func testRTCServicePathToTestClientOptions(testRTCServicePath testRTCServicePath, opts *testclient.Options) {
	if opts == nil {
		return
	}

	switch testRTCServicePath {
	case testRTCServicePathv0:
		opts.RTCServicePath = "/rtc"
	case testRTCServicePathv0SinglePeerConnection:
		opts.RTCServicePath = "/rtc"
		opts.UseJoinRequestQueryParam = true
	case testRTCServicePathv1:
		opts.RTCServicePath = "/rtc/v1"
		opts.UseJoinRequestQueryParam = true
	default:
		opts.RTCServicePath = "/rtc"
	}
}

// creates a client and runs against server
func createRTCClient(name string, port int, testRTCServicePath testRTCServicePath, opts *testclient.Options) *testclient.RTCClient {
	var customizer func(token *auth.AccessToken, grants *auth.VideoGrant)
	if opts != nil {
		customizer = opts.TokenCustomizer
	}
	token := joinToken(testRoom, name, customizer)

	return createRTCClientWithToken(token, port, testRTCServicePath, opts)
}

// creates a client and runs against server
func createRTCClientWithToken(token string, port int, testRTCServicePath testRTCServicePath, opts *testclient.Options) *testclient.RTCClient {
	if opts == nil {
		opts = &testclient.Options{
			AutoSubscribe: true,
		}
	}
	testRTCServicePathToTestClientOptions(testRTCServicePath, opts)
	ws, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", port), token, opts)
	if err != nil {
		panic(err)
	}

	c, err := testclient.NewRTCClient(ws, opts.UseJoinRequestQueryParam, opts)
	if err != nil {
		panic(err)
	}

	go c.Run()

	return c
}

// redisAddr is where the multi-node test servers and redisClient look for redis.
// Tests that need to control the redis instance itself -- to simulate an outage,
// say -- point this at one they started, with useRedisAddr.
var redisAddr = "localhost:6379"

// useRedisAddr runs the calling test against a different redis instance. The
// suite's tests share fixed ports and so never run in parallel; this would need
// to become per-test state if that changed.
func useRedisAddr(t *testing.T, addr string) {
	prev := redisAddr
	redisAddr = addr
	t.Cleanup(func() { redisAddr = prev })
}

func redisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: redisAddr,
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

func stopWriters(writers ...testclient.TrackWriter) {
	for _, w := range writers {
		w.Stop()
	}
}

func stopClients(clients ...*testclient.RTCClient) {
	for _, c := range clients {
		c.Stop()
	}
}

// -----------------------------------------------------------------------------
// vnet media harness
//
// Setup specific to driving a real server transport over a virtual network. The
// pion side lives in pkg/testutils/vnettest, shared with the pkg/sfu media tests.
// -----------------------------------------------------------------------------

// newVNetWebRTCConfig builds the server side WebRTCConfig on net. The direction
// configs come from the production NewWebRTCConfig so the negotiated extensions and
// feedback stay in step with it; the setting engine is replaced so no real socket or
// ICE mux is bound.
func newVNetWebRTCConfig(t *testing.T, net *vnet.Net, bufferFactory *buffer.Factory) *rtc.WebRTCConfig {
	t.Helper()

	conf, err := config.NewConfig("", true, nil, nil)
	require.NoError(t, err)

	// an ephemeral port range instead of the dev mode single port, which would bind a mux
	conf.RTC.TCPPort = 0
	conf.RTC.UDPPort = rtcconfig.PortRange{}
	conf.RTC.ICEPortRangeStart = 50000
	conf.RTC.ICEPortRangeEnd = 60000

	rtcConf, err := rtc.NewWebRTCConfig(conf)
	require.NoError(t, err)
	require.Nil(t, rtcConf.UDPMux, "test config must not bind a udp mux")

	rtcConf.SettingEngine = vnettest.NewSettingEngine(net)
	rtcConf.SetBufferFactory(bufferFactory)

	return rtcConf
}

// stripDeclaredSSRCs removes the a=ssrc lines pion puts in its offer. Browsers doing
// rid based simulcast do not declare per-layer SSRCs, which is why a repair SSRC has to
// be learned at all; leaving them in would let the receiver resolve everything from SDP.
func stripDeclaredSSRCs(offer string) string {
	lines := strings.Split(offer, "\r\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "a=ssrc") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\r\n")
}

// sendUntil calls send every 20ms until done reports true or the timeout expires,
// returning the final state of done.
func sendUntil(t *testing.T, timeout time.Duration, done func() bool, send func()) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return true
		}
		send()
		time.Sleep(20 * time.Millisecond)
	}
	return done()
}
