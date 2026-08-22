// Copyright 2026 LiveKit, Inc.
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

package service_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/endpoint"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/conformance"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
)

const (
	testKey    = "test"
	testSecret = "verysecretsecret"
)

type endpointStack struct {
	t   *testing.T
	ts  *httptest.Server
	svc *service.AgentService
}

func newEndpointStack(t *testing.T, endpointsCfg agent.EndpointsConfig) *endpointStack {
	localNode, err := routing.NewLocalNode(nil)
	require.NoError(t, err)
	keyProvider := auth.NewSimpleKeyProvider(testKey, testSecret)

	svc, err := service.NewAgentService(
		&config.Config{
			Region: "test",
			Keys:   map[string]string{testKey: testSecret},
			Agents: agent.Config{TargetLoad: agent.DefaultTargetLoad, Endpoints: endpointsCfg},
		},
		localNode,
		psrpc.NewLocalMessageBus(),
		keyProvider,
	)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.Handle("/agent", svc)
	mux.Handle(endpoint.PathPrefix, svc.EndpointFront())

	authMW := service.NewAPIKeyAuthMiddleware(keyProvider)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMW.ServeHTTP(w, r, mux.ServeHTTP)
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { svc.DrainConnections(time.Millisecond, true) })

	return &endpointStack{t: t, ts: ts, svc: svc}
}

func (s *endpointStack) wsURL() string {
	return "ws" + strings.TrimPrefix(s.ts.URL, "http") + "/agent"
}

func (s *endpointStack) startWorker(target string, deployment string, endpoints []*livekit.AgentHttp_AgentEndpoint) *conformance.Worker {
	w := conformance.New(conformance.Config{
		ServerURL:  s.wsURL(),
		APIKey:     testKey,
		APISecret:  testSecret,
		AgentName:  "test-agent",
		Deployment: deployment,
		Endpoints:  endpoints,
		TargetAddr: strings.TrimPrefix(target, "http://"),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(s.t, w.Start(ctx))
	s.t.Cleanup(w.Close)
	return w
}

func (s *endpointStack) clientToken(t *testing.T) string {
	at := auth.NewAccessToken(testKey, testSecret).SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: "x"})
	tok, err := at.ToJWT()
	require.NoError(t, err)
	return tok
}

func httpEP(path string, methods []string, public bool) *livekit.AgentHttp_AgentEndpoint {
	return &livekit.AgentHttp_AgentEndpoint{Path: path, Methods: methods, Public: public}
}

// newTargetApp is the local app the worker bridges into; it never listens on a
// port reachable through the stack, only via the tunnel.
func newTargetApp(t *testing.T, mux *http.ServeMux) *httptest.Server {
	app := httptest.NewServer(mux)
	t.Cleanup(app.Close)
	return app
}

func TestAgentEndpointsCorrectnessGate(t *testing.T) {
	bigDown := make([]byte, 4<<20)
	_, _ = rand.Read(bigDown)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.New()
		n, err := io.Copy(sum, r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "%d:%x", n, sum.Sum(nil))
	})
	mux.HandleFunc("GET /big", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bigDown)
	})
	mux.HandleFunc("GET /sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: event-%d\n\n", i)
			f.Flush()
			time.Sleep(150 * time.Millisecond)
		}
	})
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/json", []string{"GET"}, true),
		httpEP("/upload", []string{"POST"}, true),
		httpEP("/big", []string{"GET"}, true),
		httpEP("/sse", []string{"GET"}, true),
	})

	base := stack.ts.URL + "/agents/production"

	t.Run("json round trip", func(t *testing.T) {
		resp, err := http.Get(base + "/json")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		require.JSONEq(t, `{"ok":true}`, string(body))
	})

	t.Run("upload byte exact", func(t *testing.T) {
		up := make([]byte, 8<<20)
		_, _ = rand.Read(up)
		resp, err := http.Post(base+"/upload", "application/octet-stream", bytes.NewReader(up))
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		require.Equal(t, fmt.Sprintf("%d:%x", len(up), sha256.Sum256(up)), string(body))
	})

	t.Run("download byte exact", func(t *testing.T) {
		resp, err := http.Get(base + "/big")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.True(t, bytes.Equal(bigDown, body))
	})

	t.Run("sse events arrive one at a time", func(t *testing.T) {
		start := time.Now()
		resp, err := http.Get(base + "/sse")
		require.NoError(t, err)
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		var arrivals []time.Duration
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			if strings.HasPrefix(line, "data:") {
				arrivals = append(arrivals, time.Since(start))
			}
		}
		require.Len(t, arrivals, 5)
		// incremental delivery: the first event arrives well before the last is
		// even written (5 x 150ms); buffering the whole body would collapse gaps
		require.Less(t, arrivals[0], 450*time.Millisecond)
		require.Greater(t, arrivals[4]-arrivals[0], 300*time.Millisecond)
	})

	t.Run("32 concurrent requests", func(t *testing.T) {
		errCh := make(chan error, 32)
		for i := 0; i < 32; i++ {
			go func() {
				resp, err := http.Get(base + "/json")
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != 200 {
						err = fmt.Errorf("status %d", resp.StatusCode)
					}
				}
				errCh <- err
			}()
		}
		for i := 0; i < 32; i++ {
			require.NoError(t, <-errCh)
		}
	})
}

func TestAgentEndpointsStatusMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/hook", []string{"POST"}, true),
		httpEP("/private", []string{"GET"}, false),
	})

	base := stack.ts.URL + "/agents/production"

	t.Run("404 unknown path", func(t *testing.T) {
		resp, _ := http.Get(base + "/nope")
		resp.Body.Close()
		require.Equal(t, 404, resp.StatusCode)
	})
	t.Run("405 wrong method", func(t *testing.T) {
		resp, _ := http.Get(base + "/hook")
		resp.Body.Close()
		require.Equal(t, 405, resp.StatusCode)
	})
	t.Run("401 non-public without token", func(t *testing.T) {
		resp, _ := http.Get(base + "/private")
		resp.Body.Close()
		require.Equal(t, 401, resp.StatusCode)
	})
	t.Run("200 non-public with token", func(t *testing.T) {
		req, _ := http.NewRequest("GET", base+"/private", nil)
		req.Header.Set("Authorization", "Bearer "+stack.clientToken(t))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
	})
	t.Run("503 unknown deployment", func(t *testing.T) {
		resp, _ := http.Get(stack.ts.URL + "/agents/staging/hook")
		resp.Body.Close()
		require.Equal(t, 503, resp.StatusCode)
	})
	t.Run("307 slash redirect", func(t *testing.T) {
		c := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := c.Post(base+"/hook/", "text/plain", nil)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, 307, resp.StatusCode)
		require.Equal(t, "/agents/production/hook", resp.Header.Get("Location"))
	})
}

func TestAgentEndpointsHOL(t *testing.T) {
	// one data conn forces every stream onto the same socket: the credit windows
	// and the write scheduler are the only things standing between a stalled
	// reader and its siblings
	blocked := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /drip", func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		buf := make([]byte, 64<<10)
		for {
			if _, err := w.Write(buf); err != nil {
				return
			}
			f.Flush()
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	})
	mux.HandleFunc("GET /quick", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{DataConnCount: 1})
	stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/drip", []string{"GET"}, true),
		httpEP("/quick", []string{"GET"}, true),
	})
	base := stack.ts.URL + "/agents/production"

	// a stalled client: open /drip, read a little, then stop reading entirely
	resp, err := http.Get(base + "/drip")
	require.NoError(t, err)
	defer resp.Body.Close()
	small := make([]byte, 1024)
	_, err = io.ReadFull(resp.Body, small)
	require.NoError(t, err)
	// do not read further; the stream's credit window fills and stays full
	close(blocked)
	time.Sleep(500 * time.Millisecond)

	// sibling streams on the SAME connection must proceed at full speed
	for i := 0; i < 5; i++ {
		start := time.Now()
		q, err := http.Get(base + "/quick")
		require.NoError(t, err)
		body, _ := io.ReadAll(q.Body)
		q.Body.Close()
		require.Equal(t, "ok", string(body))
		require.Less(t, time.Since(start), 2*time.Second,
			"sibling stream stalled behind a blocked heavy stream")
	}
}

func TestAgentEndpointsRetrySafety(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ok"))
	})
	app := newTargetApp(t, mux)

	// a worker whose local app is unreachable REFUSES streams; the front must
	// retry on the healthy worker exactly once
	stack := newEndpointStack(t, agent.EndpointsConfig{})
	deadTarget := "127.0.0.1:1" // nothing listens
	eps := []*livekit.AgentHttp_AgentEndpoint{httpEP("/json", []string{"GET"}, true)}

	broken := conformance.New(conformance.Config{
		ServerURL: stack.wsURL(), APIKey: testKey, APISecret: testSecret,
		AgentName: "test-agent", Deployment: "production",
		Endpoints: eps, TargetAddr: deadTarget,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, broken.Start(ctx))
	t.Cleanup(broken.Close)

	stack.startWorker(app.URL, "production", eps)

	// run enough requests that both workers get picked first sometimes
	okCount := 0
	for i := 0; i < 12; i++ {
		resp, err := http.Get(stack.ts.URL + "/agents/production/json")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && string(body) == "ok" {
			okCount++
		}
	}
	require.Equal(t, 12, okCount, "REFUSED streams must fall through to the healthy worker")
	require.EqualValues(t, 12, hits.Load())
}

func TestAgentEndpointsTruncationAborts(t *testing.T) {
	// a worker connection dying mid-response must abort the client connection,
	// never expose a clean-looking short body
	mux := http.NewServeMux()
	release := make(chan struct{})
	mux.HandleFunc("GET /partial", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write(make([]byte, 1000))
		w.(http.Flusher).Flush()
		<-release
	})
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	w := stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/partial", []string{"GET"}, true),
	})

	resp, err := http.Get(stack.ts.URL + "/agents/production/partial")
	require.NoError(t, err)
	defer resp.Body.Close()

	head := make([]byte, 1000)
	_, err = io.ReadFull(resp.Body, head)
	require.NoError(t, err)

	w.Close() // kill the worker mid-stream
	close(release)

	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "truncated response must not read as clean EOF")
}

func TestAgentEndpointsNoLocalListenerContract(t *testing.T) {
	// the front never routes undeclared paths: the worker-local health/info
	// routes are unreachable by construction
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("local")) })
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/declared", []string{"GET"}, true),
	})

	for _, path := range []string{"/", "/worker", "/undeclared"} {
		resp, err := http.Get(stack.ts.URL + "/agents/production" + path)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, 404, resp.StatusCode, path)
	}
}

// dial helper kept for upgrade tests once the SDK lands; avoids unused imports
var _ = net.Dialer{}

// the upgrade exchange and the raw bidirectional session both ride one stream;
// this guards the front's hijack path and the no-half-close rule for upgrades
func TestAgentEndpointsWebSocketUpgrade(t *testing.T) {
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, append([]byte("echo:"), msg...)); err != nil {
				return
			}
		}
	})
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/ws", Kind: livekit.AgentHttp_AEK_WEBSOCKET, Public: true},
	})

	wsURL := "ws" + strings.TrimPrefix(stack.ts.URL, "http") + "/agents/production/ws"
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	if resp != nil {
		resp.Body.Close()
	}
	defer c.Close()

	for i := 0; i < 5; i++ {
		payload := fmt.Sprintf("msg-%d", i)
		require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte(payload)))
		_, echoed, err := c.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, "echo:"+payload, string(echoed))
	}

	// a larger frame exercises credit flow through the raw pump
	big := bytes.Repeat([]byte("x"), 256<<10)
	require.NoError(t, c.WriteMessage(websocket.BinaryMessage, big))
	_, echoed, err := c.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, len(big)+5, len(echoed))
}
