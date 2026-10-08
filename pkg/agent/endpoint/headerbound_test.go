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
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/livekit"
)

func padHeaders(t *testing.T, url string, bytes int) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	const per = 8 << 10
	for i := 0; i*per < bytes; i++ {
		req.Header.Set(fmt.Sprintf("X-Pad-%d", i), strings.Repeat("p", per))
	}
	return req
}

// A head too large to serialize is answered directly and reaches no worker: no
// worker could accept it, so spending an attempt on it only turns a permanent
// failure into a retryable-looking one.
func TestOversizedHeadIsRejectedWithoutReachingAWorker(t *testing.T) {
	var hits atomic.Int32
	addr := rawTarget(t, func(c net.Conn) {
		hits.Add(1)
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	})
	base := startFramedWorker(t, addr, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/h", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.DefaultClient.Do(padHeaders(t, base+"/h", wire.MaxRequestHeadSize+(64<<10)))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode,
		"an unservable head must not come back as a retryable 503")
	require.Empty(t, resp.Header.Get("Retry-After"), "the request can never succeed, so it must not invite a retry")
	require.Zero(t, hits.Load(), "no worker attempt may be spent on a head no worker could accept")
}

// A head comfortably inside the bound is served normally.
func TestLargeHeadWithinBoundIsServed(t *testing.T) {
	addr := rawTarget(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	})
	base := startFramedWorker(t, addr, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/h", Methods: []string{"GET"}, Public: true},
	})

	resp, err := http.DefaultClient.Do(padHeaders(t, base+"/h", 512<<10))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, "ok", string(body))
}
