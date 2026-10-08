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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit/agent"
	"github.com/livekit/protocol/logger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// getThreadMethod is AgentThreads.GetThread of cloud-protocol, called by path
// with livekit/protocol's messages so this package depends on nothing private.
const getThreadMethod = "/cloud_protocol.AgentThreads/GetThread"

// DefaultThreadLookupTimeout bounds one GetThread on the request path.
const DefaultThreadLookupTimeout = 2 * time.Second

// NewThreadResolver resolves the thread of an A2A request through the
// AgentThreads gRPC service at conn. The request names its thread as the A2A
// contextId of the message in its body; one that names none is left to the
// worker, which creates it. The lookup runs under the caller's own token, so
// the service applies the caller's scope, and follows a redirect once, so a
// merged thread places the request where the conversation continues.
func NewThreadResolver(conn grpc.ClientConnInterface, timeout time.Duration, log logger.Logger) ThreadResolver {
	if timeout <= 0 {
		timeout = DefaultThreadLookupTimeout
	}
	return func(r *http.Request, agentName, deployment string, body []byte) (string, bool) {
		contextID := contextIDOf(body)
		if contextID == "" {
			return "", false
		}
		token := requestToken(r)
		if token == "" {
			return "", false
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

		thread, err := getThread(ctx, conn, contextID)
		if err == nil && thread.GetRedirectTo() != "" {
			thread, err = getThread(ctx, conn, thread.GetRedirectTo())
		}
		if err != nil {
			// the worker resolves it from the body itself, and answers the caller
			// with the service's verdict
			log.Debugw("agent thread lookup failed; left to the worker", "error", err, "agentName", agentName, "deployment", deployment)
			return "", false
		}
		if thread.GetAgentName() != agentName {
			return "", false
		}
		return thread.GetThreadId(), true
	}
}

func getThread(ctx context.Context, conn grpc.ClientConnInterface, threadID string) (*agent.AgentThread_Thread, error) {
	req := &agent.AgentThread_GetRequest{
		Lookup: &agent.AgentThread_GetRequest_ThreadId{ThreadId: threadID},
	}
	thread := &agent.AgentThread_Thread{}
	if err := conn.Invoke(ctx, getThreadMethod, req, thread); err != nil {
		return nil, err
	}
	return thread, nil
}

// contextIDOf is the message's contextId in a JSON-RPC A2A body prefix: the
// first string under that key, read token by token so a prefix cut mid-document
// still yields it when it came before the cut.
func contextIDOf(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	var key bool
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF, or the cut: whatever came before is all there is
			return ""
		}
		s, ok := tok.(string)
		if !ok {
			key = false
			continue
		}
		if key {
			return s
		}
		key = s == "contextId"
	}
}

// requestToken is the caller's LiveKit token as the front reads it: the
// bearer, else access_token on the query for a browser that cannot set
// headers. Empty when there is none.
func requestToken(r *http.Request) string {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
		return tok
	}
	return r.URL.Query().Get("access_token")
}
