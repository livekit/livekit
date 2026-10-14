// Copyright 2026 LiveKit, Inc.

package endpoint_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent/endpoint"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/conformance"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// startWSFront serves app behind a front and a conformance worker over a real
// WebTransport session. Every request is granted.
func startWSFront(
	t *testing.T,
	app http.Handler,
	eps []*livekit.AgentHttp_AgentEndpoint,
	tune func(*endpoint.FrontParams),
) (*endpoint.Front, string) {
	t.Helper()
	target := httptest.NewServer(app)
	t.Cleanup(target.Close)

	reg, scope := endpoint.NewRegistry(), endpoint.NewScope(logger.GetLogger())
	base := startWTServer(t, reg, scope)

	params := endpoint.FrontParams{
		ResolveAccess: func(*http.Request, string, string) (endpoint.Access, bool) {
			return endpoint.Access{Scope: scope, Level: endpoint.AccessGranted}, true
		},
		Logger: logger.GetLogger(),
	}
	if tune != nil {
		tune(&params)
	}
	front := endpoint.NewFront(params)
	ts := httptest.NewServer(front)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	w := conformance.New(conformance.Config{
		ServerURL:  base,
		APIKey:     "APIkey",
		APISecret:  "secret-that-is-long-enough-to-sign",
		AgentName:  "myagent",
		Deployment: "production",
		TargetAddr: strings.TrimPrefix(target.URL, "http://"),
		Insecure:   true,
		Endpoints:  eps,
	})
	require.NoError(t, w.Start(ctx))
	t.Cleanup(w.Close)
	require.NoError(t, w.WaitRegistered(ctx))

	return front, ts.URL + "/agents/myagent/production"
}

func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

var wsRoute = []*livekit.AgentHttp_AgentEndpoint{{Path: "/ws", Methods: []string{"GET"}, Public: true}}

// wsEchoApp echoes every message on /ws and reports the close code the client sent.
func wsEchoApp(closed chan<- int) http.Handler {
	up := websocket.Upgrader{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				if closed != nil {
					var ce *websocket.CloseError
					if errors.As(err, &ce) {
						closed <- ce.Code
					} else {
						closed <- -1
					}
				}
				return
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	})
}

func TestWebSocketEchoThroughFront(t *testing.T) {
	closed := make(chan int, 1)
	front, base := startWSFront(t, wsEchoApp(closed), wsRoute, nil)

	c, resp, err := websocket.DefaultDialer.Dial(wsURL(base)+"/ws", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.Equal(t, 1, front.OpenTunnels())

	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte("hello")))
	mt, msg, err := c.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, mt)
	require.Equal(t, "hello", string(msg))

	big := []byte(strings.Repeat("b", 200<<10))
	require.NoError(t, c.WriteMessage(websocket.BinaryMessage, big))
	mt, msg, err = c.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.BinaryMessage, mt)
	require.Equal(t, big, msg)

	require.NoError(t, c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye")))
	select {
	case code := <-closed:
		require.Equal(t, websocket.CloseNormalClosure, code)
	case <-time.After(5 * time.Second):
		t.Fatal("the client's close never reached the app")
	}
	_ = c.Close()
	require.Eventually(t, func() bool { return front.OpenTunnels() == 0 }, 5*time.Second, 10*time.Millisecond)
}

// an app refusing the handshake answers like any route.
func TestWebSocketRefusedUpgradeIsOrdinaryResponse(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	front, base := startWSFront(t, app, wsRoute, nil)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(base)+"/ws", nil)
	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, "nope\n", string(body))
	require.Equal(t, 0, front.OpenTunnels())
}

func TestWebSocketDrainSendsGoingAway(t *testing.T) {
	front, base := startWSFront(t, wsEchoApp(nil), wsRoute, nil)

	c, _, err := websocket.DefaultDialer.Dial(wsURL(base)+"/ws", nil)
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte("x")))
	_, _, err = c.ReadMessage()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		front.Drain(ctx)
		close(done)
	}()

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = c.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseGoingAway), "got %v", err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return")
	}
	require.Equal(t, 0, front.OpenTunnels())

	// a draining front admits no new tunnel
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(base)+"/ws", nil)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestWebSocketTunnelLimit(t *testing.T) {
	_, base := startWSFront(t, wsEchoApp(nil), wsRoute, func(p *endpoint.FrontParams) {
		p.MaxTunnelsPerWorker = 1
	})

	c, _, err := websocket.DefaultDialer.Dial(wsURL(base)+"/ws", nil)
	require.NoError(t, err)
	defer c.Close()

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(base)+"/ws", nil)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// the worker gets identity from the preamble; the caller's LiveKit token must
// not reach application code.
func TestLiveKitCredentialsDoNotReachTheWorker(t *testing.T) {
	type seen struct {
		auth  string
		query string
	}
	got := make(chan seen, 2)
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{auth: r.Header.Get("Authorization"), query: r.URL.RawQuery}
	})
	_, base := startWSFront(t, app, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/who", Methods: []string{"GET"}, Public: true},
	}, nil)

	jwt, err := auth.NewAccessToken("APIkey", "secret-that-is-long-enough-to-sign").
		SetIdentity("caller").SetValidFor(time.Minute).ToJWT()
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, base+"/who?access_token="+jwt+"&x=1&b=%2F", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	s := <-got
	require.Empty(t, s.auth)
	require.Equal(t, "x=1&b=%2F", s.query)

	req, _ = http.NewRequest(http.MethodGet, base+"/who", nil)
	req.Header.Set("Authorization", "Bearer app-owned")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, "Bearer app-owned", (<-got).auth)
}

// an HTTP/1 client keeps streaming its body after the response has started.
func TestFullDuplexOverHTTP1(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		_, _ = io.WriteString(w, "ready\n")
		_ = http.NewResponseController(w).Flush()
		br := bufio.NewReader(r.Body)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				_, _ = io.WriteString(w, "echo "+line)
				_ = http.NewResponseController(w).Flush()
			}
			if err != nil {
				return
			}
		}
	})
	_, base := startWSFront(t, app, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/duplex", Methods: []string{"POST"}, Public: true},
	}, nil)

	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPost, base+"/duplex", pr)
	var (
		resp *http.Response
		err  error
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// bounds the test if the second echo never comes
		resp, err = (&http.Client{Timeout: 10 * time.Second}).Do(req)
	}()
	_, _ = io.WriteString(pw, "one\n")
	wg.Wait()
	require.NoError(t, err)
	defer resp.Body.Close()
	rr := bufio.NewReader(resp.Body)

	line, err := rr.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	line, err = rr.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "echo one\n", line)

	// the response is streaming; the request body is still open
	_, err = io.WriteString(pw, "two\n")
	require.NoError(t, err)
	line, err = rr.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "echo two\n", line)

	require.NoError(t, pw.Close())
	rest, err := io.ReadAll(rr)
	require.NoError(t, err)
	require.Empty(t, string(rest))
}
