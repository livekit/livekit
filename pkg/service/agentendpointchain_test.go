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
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

// rawHTTPTarget speaks HTTP/1.1 by hand, so it can end a response in ways
// net/http will not produce.
func rawHTTPTarget(t *testing.T, respond func(net.Conn)) string {
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

// A chunked body cut short must not reach the client as complete, and nothing in
// the serving chain may recover the abort that prevents it.
func TestAgentChainDeliversTruncatedChunkedResponse(t *testing.T) {
	addr := rawHTTPTarget(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = fmt.Fprintf(c, "%x\r\n", 1000)
		_, _ = c.Write(bytes.Repeat([]byte("y"), 1000))
		_, _ = io.WriteString(c, "\r\n")
		// closes without the terminating 0-length chunk
	})

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	stack.startWorker(addr, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/partial", []string{"GET"}, true),
	})

	resp, err := http.Get(stack.ts.URL + "/agents/test-agent/production/partial")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.Error(t, err, "a truncated chunked body must not read as a clean EOF through the serving chain")
	require.NotContains(t, string(body), "PANIC:", "recovery must not write panic text into a committed body")
	require.NotContains(t, string(body), "goroutine ", "a stack trace must never reach the client")
}

// The front streams request bodies, so its chain does not bound them.
func TestAgentChainExemptsAPIBodyLimiter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%d", n)
	})
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/upload", []string{"POST"}, true),
	})

	big := bytes.Repeat([]byte("z"), testMaxAPIBodySize*2)

	resp, err := http.Post(stack.ts.URL+"/agents/test-agent/production/upload", "application/octet-stream", bytes.NewReader(big))
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, fmt.Sprintf("%d", len(big)), string(got), "the whole body must reach the application")

	// the same body on an API route is still rejected
	apiResp, err := http.Post(stack.ts.URL+"/api-sink", "application/octet-stream", bytes.NewReader(big))
	require.NoError(t, err)
	defer apiResp.Body.Close()
	require.Equal(t, http.StatusRequestEntityTooLarge, apiResp.StatusCode)
}

// A manifest may declare PUT, so the agent chain's preflight must allow it.
func TestAgentChainCORSAllowsPUT(t *testing.T) {
	stack := newEndpointStack(t, agent.EndpointsConfig{})

	preflight := func(t *testing.T, path string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodOptions, stack.ts.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "https://example.com")
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.Header.Get("Access-Control-Allow-Methods")
	}

	require.Contains(t, preflight(t, "/agents/test-agent/production/thing"), http.MethodPut)
	require.NotContains(t, preflight(t, "/api-sink"), http.MethodPut)
}

// Disabled leaves the prefix unserved.
func TestAgentEndpointsDisabledIsNotServed(t *testing.T) {
	stack := newEndpointStack(t, agent.EndpointsConfig{Disabled: true})

	resp, err := http.Get(stack.ts.URL + "/agents/test-agent/production/json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// A leading "//" is folded above the split, so it still routes to the agent chain.
func TestAgentChainRoutesDoubleSlashedPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/json", []string{"GET"}, true),
	})

	resp, err := http.Get(stack.ts.URL + "//agents/test-agent/production/json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "ok", string(body))
}

// The front resolves access from the grants the api-key auth middleware installs,
// so the agent chain has to carry it.
func TestAgentChainStillResolvesGrants(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /private", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	app := newTargetApp(t, mux)

	stack := newEndpointStack(t, agent.EndpointsConfig{})
	stack.startWorker(app.URL, "production", []*livekit.AgentHttp_AgentEndpoint{
		httpEP("/private", []string{"GET"}, false),
	})
	url := stack.ts.URL + "/agents/test-agent/production/private"

	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a non-public route needs a grant")

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+stack.endpointToken(t, &auth.AgentEndpointGrant{
		Call: true, AgentName: "test-agent", Deployment: "production",
	}))
	granted, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer granted.Body.Close()
	require.Equal(t, http.StatusOK, granted.StatusCode, "a scoped grant must reach the application")
}
