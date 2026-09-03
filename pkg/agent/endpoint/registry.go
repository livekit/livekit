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
	"errors"
	"slices"
	"sync"
)

// DefaultDeployment is the URL segment that addresses workers registered with an
// empty deployment name (self-hosted workers typically set none).
const DefaultDeployment = "default"

// DefaultMaxStreams is the soft per-session concurrency cap used only for
// capacity weighting; QUIC's own stream limit is the hard bound.
const DefaultMaxStreams = 256

// ErrNoSession means the registration has no live data-plane session, so no
// stream can be opened toward the worker.
var ErrNoSession = errors.New("registration has no data-plane session")

func normalizeDeployment(d string) string {
	if d == "" {
		return DefaultDeployment
	}
	return d
}

// Registration is one worker's data-plane state: its manifest and the single
// WebTransport session that carries both its control stream and the HTTP
// exchanges the node opens toward it. It lives exactly as long as that session
// (epoch fencing: a reconnecting worker forms a new registration, and the old
// session dies with it).
type Registration struct {
	WorkerID   string
	APIKey     string
	AgentName  string
	Deployment string
	Manifest   *Manifest

	// Draining is provided by the control-plane layer that owns the worker; a
	// draining worker takes no new streams. Worker selection uses live in-flight
	// streams, not a reported load.
	Draining func() bool

	mu      sync.Mutex
	session Session
	closed  bool
}

// SetSession attaches the worker's live data-plane session. One session per
// worker: the WebTransport session that also carries its control stream.
func (r *Registration) SetSession(s Session) {
	r.mu.Lock()
	r.session = s
	r.mu.Unlock()
}

func (r *Registration) getSession() Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	return r.session
}

// OpenStream opens a data-plane stream toward the worker for one HTTP exchange.
func (r *Registration) OpenStream(ctx context.Context) (Stream, error) {
	s := r.getSession()
	if s == nil {
		return nil, ErrNoSession
	}
	return s.OpenStream(ctx)
}

// HasSession reports whether the worker has a live data-plane session, i.e. can
// serve HTTP requests right now.
func (r *Registration) HasSession() bool {
	return r.getSession() != nil
}

// InflightStreams reports the open streams on this worker's session - the
// least-outstanding-requests signal for worker selection.
func (r *Registration) InflightStreams() int {
	s := r.getSession()
	if s == nil {
		return 0
	}
	return s.OpenStreams()
}

// SpareStreams reports remaining stream capacity - the node's live serving
// headroom for the worker, used to weight node selection.
func (r *Registration) SpareStreams() int {
	s := r.getSession()
	if s == nil {
		return 0
	}
	if spare := s.MaxStreams() - s.OpenStreams(); spare > 0 {
		return spare
	}
	return 0
}

func (r *Registration) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	s := r.session
	r.session = nil
	r.mu.Unlock()
	if s != nil {
		s.Close("registration closed")
	}
}

// Registry tracks data-plane registrations on this node, keyed by
// (api key, agent name, deployment). The api key is the project identity in OSS.
type Registry struct {
	mu    sync.Mutex
	regs  map[string]*Registration // by worker id
	byKey map[regKey][]*Registration
}

type regKey struct {
	apiKey     string
	agentName  string
	deployment string
}

func NewRegistry() *Registry {
	return &Registry{
		regs:  make(map[string]*Registration),
		byKey: make(map[regKey][]*Registration),
	}
}

// Register records a registration. A worker id already present is superseded:
// worker ids are stable across reconnects, and the retiring session must not be
// able to strand the new epoch (its own Deregister is a no-op once replaced).
// The superseded epoch's session is closed.
func (g *Registry) Register(r *Registration) error {
	key := regKey{r.APIKey, r.AgentName, normalizeDeployment(r.Deployment)}
	g.mu.Lock()
	old := g.regs[r.WorkerID]
	if old != nil {
		g.removeLocked(old)
	}
	g.regs[r.WorkerID] = r
	g.byKey[key] = append(g.byKey[key], r)
	g.mu.Unlock()
	if old != nil {
		old.close()
	}
	return nil
}

// removeLocked unlinks a registration from all indexes. Callers hold g.mu.
func (g *Registry) removeLocked(r *Registration) {
	delete(g.regs, r.WorkerID)
	key := regKey{r.APIKey, r.AgentName, normalizeDeployment(r.Deployment)}
	if regs := g.byKey[key]; len(regs) > 0 {
		if i := slices.Index(regs, r); i != -1 {
			regs = slices.Delete(regs, i, i+1)
		}
		if len(regs) == 0 {
			delete(g.byKey, key)
		} else {
			g.byKey[key] = regs
		}
	}
}

// Deregister removes exactly this registration; it is a no-op when a newer
// epoch has already superseded it.
func (g *Registry) Deregister(r *Registration) {
	g.mu.Lock()
	if g.regs[r.WorkerID] != r {
		g.mu.Unlock()
		return
	}
	g.removeLocked(r)
	g.mu.Unlock()
	r.close()
}

// Candidates returns the registrations for (api key, agent name, deployment segment).
func (g *Registry) Candidates(apiKey, agentName, deployment string) []*Registration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.byKey[regKey{apiKey, agentName, normalizeDeployment(deployment)}])
}

// SingleAPIKey returns the api key when every registration shares one - the OSS
// resolution for unauthenticated requests to public endpoints. ok is false when
// zero or multiple keys are present.
func (g *Registry) SingleAPIKey() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var key string
	for _, r := range g.regs {
		if key == "" {
			key = r.APIKey
		} else if key != r.APIKey {
			return "", false
		}
	}
	return key, key != ""
}
