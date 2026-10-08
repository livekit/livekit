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

package service

import (
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/livekit/livekit-server/pkg/agent/endpoint"
)

// AgentEndpointService is the /agents/{agent_name}/{deployment}/{path...} front
// backed by this node's attached workers. The api key comes from validated grants
// when a token is present; a non-public route additionally requires an
// agent-endpoint grant scoped to this agent and deployment.
type AgentEndpointService struct {
	*endpoint.Front
}

func NewAgentEndpointService(h *AgentHandler, scopes *EndpointScopes) *AgentEndpointService {
	return &AgentEndpointService{
		Front: endpoint.NewFront(endpoint.FrontParams{
			ResolveAccess: func(r *http.Request, agentName, deployment string) (endpoint.Access, bool) {
				if claims := GetGrants(r.Context()); claims != nil {
					level := endpoint.AccessCredentialed
					if claims.AgentEndpoint.Allows(agentName, deployment) {
						level = endpoint.AccessGranted
					}
					return endpoint.Access{
						Scope:       scopes.Scope(GetAPIKey(r.Context()), agentName, deployment),
						Level:       level,
						ThreadGrant: claims.AgentThread,
					}, true
				}
				// unauthenticated: one configured key makes the api key
				// unambiguous, and failing that a single attached tenant does.
				// A guessed api key confers no access, so such a request still
				// reaches only routes marked public.
				apiKey := h.singleAPIKey
				if apiKey == "" {
					var ok bool
					if apiKey, ok = scopes.SingleKey(); !ok {
						return endpoint.Access{}, false
					}
				}
				return endpoint.Access{
					Scope: scopes.Scope(apiKey, agentName, deployment),
				}, true
			},
			Logger:              h.logger,
			MaxTunnelsPerWorker: int(h.endpointsConfig.MaxTunnels),
			TunnelDrainTimeout:  h.endpointsConfig.TunnelDrainTimeout,
			ResolveThread:       threadResolver(h),
		}),
	}
}

// threadResolver is the front's thread lookup when the server is configured
// with an AgentThreads address; nil otherwise, and a text route is forwarded
// unchanged.
func threadResolver(h *AgentHandler) endpoint.ThreadResolver {
	address := h.endpointsConfig.ThreadsAddress
	if address == "" {
		return nil
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		h.logger.Warnw("agent threads address is not dialable; threads are left to the workers", err, "address", address)
		return nil
	}
	return endpoint.NewThreadResolver(conn, h.endpointsConfig.ThreadLookupTimeout, h.logger)
}
