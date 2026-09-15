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

package endpoint_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent/endpoint"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/conformance"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// rawTarget speaks HTTP/1.1 by hand so a response can end in ways net/http will
// not produce.
func rawTarget(t *testing.T, respond func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				respond(c)
			}()
		}
	}()
	return ln.Addr().String()
}

func startFramedWorker(t *testing.T, targetAddr string, eps []*livekit.AgentHttp_AgentEndpoint) string {
	t.Helper()
	reg := endpoint.NewRegistry()
	base := startWTServer(t, reg)

	front := endpoint.NewFront(endpoint.FrontParams{
		Registry: reg,
		ResolveAccess: func(*http.Request, string, string) endpoint.Access {
			return endpoint.Access{}
		},
		Logger:            logger.GetLogger(),
		SingleKeyFallback: true,
	})
	ts := httptest.NewUnstartedServer(front)
	// raised past net/http's 1 MiB default so the front's own head bound is what
	// rejects an oversized head
	ts.Config.MaxHeaderBytes = 4 << 20
	ts.Start()
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	w := conformance.New(conformance.Config{
		ServerURL:  base,
		APIKey:     "APIkey",
		APISecret:  "secret-that-is-long-enough-to-sign",
		AgentName:  "myagent",
		Deployment: "production",
		TargetAddr: targetAddr,
		Insecure:   true,
		Endpoints:  eps,
	})
	require.NoError(t, w.Start(ctx))
	t.Cleanup(w.Close)
	require.NoError(t, w.WaitRegistered(ctx))

	return ts.URL + "/agents/myagent/production"
}

// A chunked response cut short must not reach the client as complete.
func TestTruncatedChunkedResponseIsDetected(t *testing.T) {
	addr := rawTarget(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = fmt.Fprintf(c, "%x\r\n", 1000)
		_, _ = c.Write(make([]byte, 1000))
		_, _ = io.WriteString(c, "\r\n")
		// closes without the terminating 0-length chunk
	})

	base := startFramedWorker(t, addr, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/partial", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.Get(base + "/partial")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "a truncated chunked body must not read as a clean EOF")
}

// A response short of its declared length must not reach the client as complete.
func TestShortContentLengthResponseIsDetected(t *testing.T) {
	addr := rawTarget(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 1000000\r\n\r\n")
		_, _ = c.Write(make([]byte, 1000))
	})

	base := startFramedWorker(t, addr, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/short", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.Get(base + "/short")
	require.NoError(t, err)
	defer resp.Body.Close()

	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "a body short of its declared length must not read as complete")
}

// An unreachable application never dispatched the request, so it is retryable;
// with no healthy worker the attempts exhaust into a 503.
func TestRefusedExhaustsTo503(t *testing.T) {
	base := startFramedWorker(t, "127.0.0.1:1", []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/json", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.Get(base + "/json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"a refused request is retryable, so it must exhaust into 503 rather than 502")
}

// A complete response reads as complete.
func TestCompleteResponseStillCompletes(t *testing.T) {
	body := strings.Repeat("y", 40<<10)
	addr := rawTarget(t, func(c net.Conn) {
		_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(body))
		_, _ = io.WriteString(c, body)
	})

	base := startFramedWorker(t, addr, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/ok", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.Get(base + "/ok")
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))
}

// A bodiless response declares a length it never delivers and is not truncated.
func TestHeadAndNoContentAreNotTruncated(t *testing.T) {
	addr := rawTarget(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "HTTP/1.1 204 No Content\r\n\r\n")
	})
	base := startFramedWorker(t, addr, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/empty", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.Get(base + "/empty")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
}

// Duplicate response headers must survive the proto hop.
func TestDuplicateHeadersSurvive(t *testing.T) {
	addr := rawTarget(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nSet-Cookie: a=1\r\nSet-Cookie: b=2\r\nContent-Length: 2\r\n\r\nhi")
	})
	base := startFramedWorker(t, addr, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/cookies", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.Get(base + "/cookies")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.ElementsMatch(t, []string{"a=1", "b=2"}, resp.Header.Values("Set-Cookie"))
}

// The request head reaches the application with path, query and forwarding
// headers intact.
func TestRequestMetadataReachesWorker(t *testing.T) {
	seen := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		var head strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			head.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		seen <- head.String()
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	}()

	base := startFramedWorker(t, ln.Addr().String(), []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/items/{id}", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.Get(base + "/items/42?q=1")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	select {
	case h := <-seen:
		require.Contains(t, h, "GET /items/42?q=1 HTTP/1.1")
		require.Contains(t, strings.ToLower(h), "x-forwarded-for:")
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the application")
	}
}
