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
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

// Relay headers carry the origin's routing decision to the winning node's relay
// listener. The relay listener binds a private address and is never reachable
// through the public front, so these headers are trusted there (the same
// network-trust model as the signal relay); the public front strips them from
// client requests regardless.
const (
	relayHeaderScope  = "X-Livekit-Agents-Scope"
	relayHeaderAuthed = "X-Livekit-Agents-Authed"
)

const (
	resolveTimeout = 2 * time.Second
	// capacity answers race under the short-circuit window, which must be
	// generous enough for cross-region holders on the shared bus
	resolveAffinityTimeout     = 500 * time.Millisecond
	resolveShortCircuitTimeout = 250 * time.Millisecond
)

// Remote is the multi-node (and, on a bus that spans regions, multi-region)
// side of the data plane: it answers ResolveEndpoint for this node's
// registrations and resolves+relays requests this node cannot serve. Request
// bytes flow over a direct HTTP hop to the winner's relay listener on the
// private network; the bus only locates.
type Remote struct {
	registry *Registry
	nodeID   string
	relayURL string
	logger   logger.Logger

	client rpc.AgentEndpointInternalClient
	server rpc.AgentEndpointInternalServer
}

func NewRemote(registry *Registry, bus psrpc.MessageBus, nodeID, relayURL string, log logger.Logger) (*Remote, error) {
	client, err := rpc.NewAgentEndpointInternalClient(bus)
	if err != nil {
		return nil, err
	}
	r := &Remote{
		registry: registry,
		nodeID:   nodeID,
		relayURL: relayURL,
		logger:   log.WithComponent("agents.endpoint.remote"),
		client:   client,
	}
	server, err := rpc.NewAgentEndpointInternalServer(r, bus)
	if err != nil {
		return nil, err
	}
	r.server = server

	// a node answers resolves for a scope exactly while it holds registrations
	// for it
	registry.setScopeHooks(
		func(scope string) {
			if err := server.RegisterResolveEndpointTopic(scope); err != nil {
				r.logger.Errorw("failed to register endpoint resolve topic", err, "scope", scope)
			}
		},
		server.DeregisterResolveEndpointTopic,
	)
	return r, nil
}

func (r *Remote) NodeID() string { return r.nodeID }

func (r *Remote) Close() {
	r.server.Shutdown()
}

// Resolve asks every node holding registrations for the scope; the manifest
// match and capacity weighting run server-side in the affinity function.
func (r *Remote) Resolve(ctx context.Context, req *rpc.ResolveEndpointRequest) (*rpc.ResolveEndpointResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	return r.client.ResolveEndpoint(ctx, req.Scope, req, psrpc.WithSelectionOpts(psrpc.SelectionOpts{
		MinimumAffinity:     presenceAffinity / 10,
		AffinityTimeout:     resolveAffinityTimeout,
		ShortCircuitTimeout: resolveShortCircuitTimeout,
	}))
}

// --- server side (AgentEndpointInternalServerImpl) ---

func (r *Remote) ResolveEndpointAffinity(ctx context.Context, req *rpc.ResolveEndpointRequest) float32 {
	return r.evaluate(req).score
}

func (r *Remote) ResolveEndpoint(ctx context.Context, req *rpc.ResolveEndpointRequest) (*rpc.ResolveEndpointResponse, error) {
	ev := r.evaluate(req)
	if ev.score < matchAffinityFloor {
		// this node holds the deployment but cannot serve the request; the
		// typed code lets the origin surface the precise status instead of a
		// generic no-workers 503 (FailedPrecondition stands in for 405)
		switch {
		case ev.saturated:
			return nil, psrpc.NewErrorf(psrpc.Unavailable, "no worker capacity")
		case ev.restricted:
			return nil, psrpc.NewErrorf(psrpc.Unauthenticated, "authentication required")
		case ev.partial:
			return nil, psrpc.NewErrorf(psrpc.FailedPrecondition, "method not allowed")
		default:
			return nil, psrpc.NewErrorf(psrpc.NotFound, "no matching endpoint")
		}
	}
	return &rpc.ResolveEndpointResponse{
		NodeId:   r.nodeID,
		RelayUrl: r.relayURL,
	}, nil
}

// Miss tiers: a node that holds the deployment but cannot serve the request
// still answers, so the origin gets a typed status (404/405/401/503) instead of
// a bus-level no-servers error. All misses claim the same tiny affinity band on
// every node, so the ordering below decides which miss wins a mixed fleet: the
// most informative one (saturated 503 beats a manifest-skew 405). Real matches
// score at least matchAffinityFloor and always win.
const (
	presenceAffinity   = float32(0.00001)
	partialAffinity    = 2 * presenceAffinity
	restrictedAffinity = 3 * presenceAffinity
	saturatedAffinity  = 4 * presenceAffinity
	matchAffinityFloor = float32(0.001)
)

type evaluation struct {
	score      float32
	partial    bool
	restricted bool
	// a full, authorized match exists but every matching worker is draining or
	// has no attached data connections
	saturated bool
}

// evaluate scores this node for a resolve: the best capacity-weighted match
// wins.
func (r *Remote) evaluate(req *rpc.ResolveEndpointRequest) (ev evaluation) {
	candidates := r.registry.Candidates(req.Scope, req.Deployment)
	if len(candidates) > 0 {
		ev.score = presenceAffinity
	}
	for _, reg := range candidates {
		route, res := reg.Manifest.Match(req.Path, req.Method, req.Websocket)
		switch res {
		case MatchPartial:
			ev.partial = true
			continue
		case MatchNone:
			continue
		}
		if !req.Authenticated && !route.Public {
			ev.restricted = true
			continue
		}
		if reg.AttachedConns() == 0 || (reg.Draining != nil && reg.Draining()) {
			ev.saturated = true
			continue
		}
		s := float32(0.001)
		if reg.Load != nil {
			s += 0.98 * max(0, 1-reg.Load())
		} else {
			s += 0.49
		}
		if s > ev.score {
			ev.score = s
		}
	}
	if ev.score < matchAffinityFloor {
		switch {
		case ev.saturated:
			ev.score = saturatedAffinity
		case ev.restricted:
			ev.score = restrictedAffinity
		case ev.partial:
			ev.score = partialAffinity
		}
	}
	return ev
}

// relay proxies one request to the winning node's relay listener, streaming
// both directions (SSE/chunked flushed per chunk, WS upgrades pass through).
func (r *Remote) relay(w http.ResponseWriter, req *http.Request, resp *rpc.ResolveEndpointResponse, scope string, authenticated bool) {
	target, err := url.Parse(resp.RelayUrl)
	if err != nil {
		http.Error(w, "bad relay target", http.StatusBadGateway)
		return
	}
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = req.URL.Path
			pr.Out.URL.RawQuery = req.URL.RawQuery
			pr.Out.Host = req.Host
			pr.Out.Header.Set(relayHeaderScope, scope)
			if authenticated {
				pr.Out.Header.Set(relayHeaderAuthed, "1")
			}
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			r.logger.Warnw("endpoint relay failed", err, "targetNodeID", resp.NodeId)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	}
	proxy.ServeHTTP(w, req.Clone(req.Context()))
}
