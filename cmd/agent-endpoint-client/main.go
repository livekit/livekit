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

// agent-endpoint-client is the reference sidecar for the agent HTTP endpoints
// data plane: it registers a manifest against a livekit-server and bridges
// tunnel streams to any local HTTP server, standing in for the SDK's tunnel
// client until it lands.
//
// Example, against a dev server:
//
//	livekit-server --dev &
//	python app.py  # any local HTTP server on :8080
//	agent-endpoint-client -url ws://localhost:7880/agent -api-key devkey \
//	  -api-secret secret -deployment production -target 127.0.0.1:8080 \
//	  -route "GET /json" -route "public GET /sse"
//	curl http://localhost:7880/agents/production/json
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/client"
)

type routeFlags []string

func (r *routeFlags) String() string     { return strings.Join(*r, ",") }
func (r *routeFlags) Set(v string) error { *r = append(*r, v); return nil }

func main() {
	var (
		url        = flag.String("url", "ws://localhost:7880/agent", "livekit-server /agent URL")
		apiKey     = flag.String("api-key", "devkey", "API key")
		apiSecret  = flag.String("api-secret", "secret", "API secret")
		agentName  = flag.String("agent-name", "endpoint-sidecar", "agent name")
		deployment = flag.String("deployment", "", "deployment name (empty = default)")
		target     = flag.String("target", "127.0.0.1:8080", "local HTTP server to bridge into")
		routes     routeFlags
	)
	flag.Var(&routes, "route", `route to expose, e.g. "GET /json", "public POST /sms", repeatable`)
	flag.Parse()

	if len(routes) == 0 {
		fmt.Fprintln(os.Stderr, "at least one -route is required")
		os.Exit(1)
	}

	var endpoints []*livekit.AgentHttp_AgentEndpoint
	for _, r := range routes {
		parts := strings.Fields(r)
		public := false
		if len(parts) > 0 && parts[0] == "public" {
			public = true
			parts = parts[1:]
		}
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "invalid -route %q, want \"[public] METHOD /path\"\n", r)
			os.Exit(1)
		}
		endpoints = append(endpoints, &livekit.AgentHttp_AgentEndpoint{
			Path:    parts[1],
			Methods: []string{strings.ToUpper(parts[0])},
			Public:  public,
		})
	}

	logger.InitFromConfig(&logger.Config{Level: "info"}, "agent-endpoint-client")

	w := client.New(client.Config{
		ServerURL:  *url,
		APIKey:     *apiKey,
		APISecret:  *apiSecret,
		AgentName:  *agentName,
		Deployment: *deployment,
		Endpoints:  endpoints,
		TargetAddr: *target,
		Logger:     logger.GetLogger(),
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "start failed:", err)
		os.Exit(1)
	}
	logger.Infow("endpoint sidecar attached", "workerID", w.WorkerID(), "routes", len(endpoints))

	<-ctx.Done()
	w.Close()
}
