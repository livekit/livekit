// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const wsSwitchingHead = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

// scriptedTunnelFront serves one scripted worker on a GET route, with a drain
// signal the test controls.
func scriptedTunnelFront(t *testing.T, draining *atomic.Bool, params FrontParams, script func(r io.Reader, w *workerSide)) *httptest.Server {
	t.Helper()
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/ws", Methods: []string{"GET"}, Public: true},
	})
	require.NoError(t, err)
	g, s := NewRegistry(), NewScope(logger.GetLogger())
	g.Register(s, NewRegistration(RegistrationParams{
		WorkerID: "w1",
		Manifest: m,
		Session:  &scriptedSession{script: script},
		Draining: draining.Load,
	}))
	params.ResolveAccess = grantedTo(s)
	params.Logger = logger.GetLogger()
	ts := httptest.NewServer(NewFront(params))
	t.Cleanup(ts.Close)
	return ts
}

// readWorkerRequest parses the request the worker was handed.
func readWorkerRequest(r io.Reader) (*http.Request, error) {
	if _, err := wire.ReadPreamble(r); err != nil {
		return nil, err
	}
	return http.ReadRequest(bufio.NewReader(r))
}

// rawWebSocketHandshake sends a handshake with the given Upgrade token and returns the
// connection positioned after the response head.
func rawWebSocketHandshake(t *testing.T, ts *httptest.Server, upgrade string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = io.WriteString(c, "GET "+PathPrefix+"a/d/ws HTTP/1.1\r\nHost: x\r\n"+
		"Connection: Upgrade\r\nUpgrade: "+upgrade+"\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	return c, br, resp
}

func TestUpgradeHeadersReachTheWorker(t *testing.T) {
	seen := make(chan http.Header, 1)
	ts := scriptedTunnelFront(t, &atomic.Bool{}, FrontParams{}, func(r io.Reader, w *workerSide) {
		defer w.Close()
		req, err := readWorkerRequest(r)
		if err != nil {
			return
		}
		seen <- req.Header
		w.WriteString(wsSwitchingHead)
	})
	_, _, resp := rawWebSocketHandshake(t, ts, "WebSocket")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	h := <-seen
	require.Equal(t, "Upgrade", h.Get("Connection"))
	require.Equal(t, "websocket", h.Get("Upgrade"))
	require.Equal(t, "dGhlIHNhbXBsZSBub25jZQ==", h.Get("Sec-WebSocket-Key"))
	require.Equal(t, "13", h.Get("Sec-WebSocket-Version"))
}

// only websocket is tunneled: any other token is stripped like any hop-by-hop
// header.
func TestNonWebSocketUpgradeIsStripped(t *testing.T) {
	seen := make(chan http.Header, 1)
	ts := scriptedTunnelFront(t, &atomic.Bool{}, FrontParams{}, func(r io.Reader, w *workerSide) {
		defer w.Close()
		req, err := readWorkerRequest(r)
		if err != nil {
			return
		}
		seen <- req.Header
		w.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	})
	_, _, resp := rawWebSocketHandshake(t, ts, "h2c")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	h := <-seen
	require.Empty(t, h.Values("Connection"))
	require.Empty(t, h.Values("Upgrade"))
}

// HTTP/2 has no Upgrade: the same headers on an h2 request are a plain GET.
func TestHTTP2UpgradeHeadersArePlainGET(t *testing.T) {
	seen := make(chan http.Header, 1)
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{{Path: "/ws", Methods: []string{"GET"}, Public: true}})
	require.NoError(t, err)
	g, s := NewRegistry(), NewScope(logger.GetLogger())
	g.Register(s, NewRegistration(RegistrationParams{WorkerID: "w1", Manifest: m, Session: &scriptedSession{
		script: func(r io.Reader, w *workerSide) {
			defer w.Close()
			req, err := readWorkerRequest(r)
			if err != nil {
				return
			}
			seen <- req.Header
			w.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		},
	}}))
	f := NewFront(FrontParams{ResolveAccess: grantedTo(s), Logger: logger.GetLogger()})

	r := httptest.NewRequest(http.MethodGet, PathPrefix+"a/d/ws", nil)
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/2.0", 2, 0
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", rec.Body.String())
	h := <-seen
	require.Empty(t, h.Values("Connection"))
	require.Empty(t, h.Values("Upgrade"))
}

// a switch the front did not ask for is a protocol error; GET is idempotent, so
// with one worker it exhausts its retries into a 503.
func TestUnrequestedSwitchIsProtocolError(t *testing.T) {
	cases := map[string]struct {
		upgrade bool
		resp    string
	}{
		"101 to a plain GET":      {resp: wsSwitchingHead},
		"101 to another protocol": {upgrade: true, resp: "HTTP/1.1 101 Switching Protocols\r\nUpgrade: h2c\r\nConnection: Upgrade\r\n\r\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ts := scriptedTunnelFront(t, &atomic.Bool{}, FrontParams{}, func(r io.Reader, w *workerSide) {
				defer w.Close()
				if _, err := readWorkerRequest(r); err != nil {
					return
				}
				w.WriteString(tc.resp)
			})
			if tc.upgrade {
				_, _, resp := rawWebSocketHandshake(t, ts, "websocket")
				require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
				return
			}
			resp, err := http.Get(ts.URL + PathPrefix + "a/d/ws")
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		})
	}
}

// a tunnel outlives its worker starting to drain by the drain timeout, then
// closes with 1001.
func TestWorkerDrainClosesTunnelWithGoingAway(t *testing.T) {
	var draining atomic.Bool
	ts := scriptedTunnelFront(t, &draining, FrontParams{TunnelDrainTimeout: 10 * time.Millisecond},
		func(r io.Reader, w *workerSide) {
			defer w.Close()
			if _, err := readWorkerRequest(r); err != nil {
				return
			}
			// one complete text frame "hi", then the tunnel idles
			w.WriteString(wsSwitchingHead + "\x81\x02hi")
			_, _ = io.Copy(io.Discard, r)
		})
	_, br, resp := rawWebSocketHandshake(t, ts, "websocket")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	frame := make([]byte, 4)
	_, err := io.ReadFull(br, frame)
	require.NoError(t, err)
	require.Equal(t, "\x81\x02hi", string(frame))

	draining.Store(true)
	_, err = io.ReadFull(br, frame)
	require.NoError(t, err)
	require.Equal(t, wsGoingAwayCloseFrame, frame)
	_, err = br.ReadByte()
	require.Error(t, err)
}

func TestWSFrameTrackerBoundaries(t *testing.T) {
	var f wsFrameTracker
	require.True(t, f.atBoundary())

	small := []byte{0x81, 0x03, 'a', 'b', 'c'}
	for i := range small {
		f.advance(small[i : i+1])
		require.Equal(t, i == len(small)-1, f.atBoundary(), "byte %d", i)
	}

	// 16-bit length, split mid-header and mid-payload
	medium := append([]byte{0x82, 126, 0x01, 0x00}, make([]byte, 256)...)
	f.advance(medium[:3])
	require.False(t, f.atBoundary())
	f.advance(medium[3:100])
	require.False(t, f.atBoundary())
	f.advance(medium[100:])
	require.True(t, f.atBoundary())

	// 64-bit length with the mask bit, back to back with a small frame
	large := append([]byte{0x82, 0xff, 0, 0, 0, 0, 0, 0, 0x01, 0x00, 1, 2, 3, 4}, make([]byte, 65536)...)
	f.advance(append(large, small...))
	require.True(t, f.atBoundary())

	f.advance([]byte{0x88, 0x00})
	require.True(t, f.atBoundary())
}
