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
	"sync"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/agent/endpoint"
)

// endpointScopeKey is this server's tenancy for agent HTTP endpoints: the api
// key is the project identity. The endpoint package holds no tenancy identity of
// its own, so this is the one place that rule is written.
type endpointScopeKey struct {
	apiKey     string
	agentName  string
	deployment string
}

func newEndpointScopeKey(apiKey, agentName, deployment string) endpointScopeKey {
	return endpointScopeKey{apiKey, agentName, endpoint.NormalizeDeployment(deployment)}
}

// EndpointScopes maps that tenancy onto the endpoint package's scopes, one per
// key, held for as long as a worker holds it.
type EndpointScopes struct {
	logger logger.Logger

	mu     sync.Mutex
	scopes map[endpointScopeKey]*endpointScopeEntry
}

// endpointScopeEntry refcounts a scope by the registrations that hold it: a
// scope must outlive a worker that has acquired it but not yet registered, or a
// concurrent release would strand the new registration in an orphaned scope.
type endpointScopeEntry struct {
	scope *endpoint.Scope
	n     int
}

func NewEndpointScopes() *EndpointScopes {
	return &EndpointScopes{
		logger: logger.GetLogger().WithComponent("agents.endpoint"),
		scopes: make(map[endpointScopeKey]*endpointScopeEntry),
	}
}

// acquire returns the key's scope, creating it on first use. Every acquire must
// be matched by a release.
func (s *EndpointScopes) acquire(k endpointScopeKey) *endpoint.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.scopes[k]
	if e == nil {
		e = &endpointScopeEntry{scope: endpoint.NewScope(s.logger.WithValues(
			"apiKey", k.apiKey, "agentName", k.agentName, "deployment", k.deployment))}
		s.scopes[k] = e
	}
	e.n++
	return e.scope
}

func (s *EndpointScopes) release(k endpointScopeKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.scopes[k]
	if e == nil {
		return
	}
	if e.n--; e.n <= 0 {
		delete(s.scopes, k)
	}
}

// Scope returns the deployment's serving state, or nil when no worker holds it -
// which the front reads as "no worker for this deployment".
func (s *EndpointScopes) Scope(apiKey, agentName, deployment string) *endpoint.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.scopes[newEndpointScopeKey(apiKey, agentName, deployment)]; e != nil {
		return e.scope
	}
	return nil
}

// SingleKey returns the api key when every scope on this node belongs to one -
// the resolution for unauthenticated requests to public endpoints. It is sound
// only because a guessed api key confers no access: the front still serves such
// a request only against routes marked public. ok is false when zero or multiple
// keys are present.
func (s *EndpointScopes) SingleKey() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var key string
	for k := range s.scopes {
		if key == "" {
			key = k.apiKey
		} else if key != k.apiKey {
			return "", false
		}
	}
	return key, key != ""
}
