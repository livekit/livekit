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

package endpoint

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit/agent"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeThreads answers GetThread for the threads it holds, by id, and records
// the authorization it was called with. It serves the method by path, the way
// the resolver calls it, so no generated service stub is needed.
type fakeThreads struct {
	threads map[string]*agent.AgentThread_Thread
	auth    []string
}

func (f *fakeThreads) serve(t *testing.T) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		if method != getThreadMethod {
			return status.Error(codes.Unimplemented, method)
		}
		md, _ := metadata.FromIncomingContext(stream.Context())
		f.auth = append(f.auth, strings.Join(md.Get("authorization"), ","))
		req := &agent.AgentThread_GetRequest{}
		if err := stream.RecvMsg(req); err != nil {
			return err
		}
		thread, ok := f.threads[req.GetThreadId()]
		if !ok {
			return status.Error(codes.NotFound, req.GetThreadId())
		}
		return stream.SendMsg(thread)
	}))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

// a body naming a thread places the request on it, a merged thread on the one
// it continues in, under the caller's own token; a body naming none, a thread
// of another agent, or a lookup that fails is left to the worker.
func TestThreadResolverPlacesByContextIdUnderTheCallersToken(t *testing.T) {
	fake := &fakeThreads{threads: map[string]*agent.AgentThread_Thread{
		"AT_1": {ThreadId: "AT_1", AgentName: "a"},
		"AT_2": {ThreadId: "AT_2", AgentName: "a", RedirectTo: "AT_3"},
		"AT_3": {ThreadId: "AT_3", AgentName: "a"},
		"AT_9": {ThreadId: "AT_9", AgentName: "other"},
	}}
	resolve := NewThreadResolver(fake.serve(t), 0, logger.GetLogger())
	body := func(id string) []byte {
		return []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"m","contextId":"` + id + `","parts":[{"kind":"text","text":"hi"}]}}}`)
	}
	req := func(tok string) *http.Request {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://node/agents/a/d/a2a", nil)
		require.NoError(t, err)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return r
	}

	id, ok := resolve(req("tok"), "a", "d", body("AT_1"))
	require.True(t, ok)
	require.Equal(t, "AT_1", id)
	require.Equal(t, []string{"Bearer tok"}, fake.auth)

	id, ok = resolve(req("tok"), "a", "d", body("AT_2"))
	require.True(t, ok, "a merged thread is followed once")
	require.Equal(t, "AT_3", id)

	_, ok = resolve(req("tok"), "a", "d", body("AT_missing"))
	require.False(t, ok, "an unknown thread is the worker's to refuse")
	_, ok = resolve(req("tok"), "a", "d", body("AT_9"))
	require.False(t, ok, "another agent's thread is not placed")
	_, ok = resolve(req("tok"), "a", "d", []byte(`{"jsonrpc":"2.0","params":{"message":{"messageId":"m"}}}`))
	require.False(t, ok, "no contextId: the worker creates the thread")
	_, ok = resolve(req(""), "a", "d", body("AT_1"))
	require.False(t, ok, "no token to look it up under")

	// the peek may cut the body: a contextId before the cut still places it
	cut := body("AT_1")[:100]
	require.Contains(t, string(cut), `"contextId":"AT_1"`)
	id, ok = resolve(req("tok"), "a", "d", cut)
	require.True(t, ok)
	require.Equal(t, "AT_1", id)
}
