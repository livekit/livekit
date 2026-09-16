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
	"sync"
)

// DefaultDeployment is the URL segment that addresses workers registered with an
// empty deployment name (self-hosted workers typically set none).
const DefaultDeployment = "default"

// UnnamedAgentSegment is the URL segment that addresses workers registered with
// an empty agent name.
const UnnamedAgentSegment = "_"

// IsReservedAgentName reports whether a name cannot address a worker. These are
// unreserved characters (RFC 3986 §2.3), so percent-encoding them yields no
// distinct form to address them by.
func IsReservedAgentName(agentName string) bool {
	return agentName == UnnamedAgentSegment || agentName == "." || agentName == ".."
}

// NormalizeDeployment maps an empty deployment to the segment that addresses it
// in a URL. Every embedder must key its scopes through this, or a worker that
// registered without a deployment is unreachable from any node but its own.
func NormalizeDeployment(d string) string {
	if d == "" {
		return DefaultDeployment
	}
	return d
}

// DefaultMaxStreams is the soft per-session concurrency cap used only for
// capacity weighting; QUIC's own stream limit is the hard bound.
const DefaultMaxStreams = 256

// ErrNoSession means the registration has no live data-plane session, so no
// stream can be opened toward the worker.
var ErrNoSession = errors.New("registration has no data-plane session")

// Registration is one worker's data-plane state: its manifest and the single
// WebTransport session that carries both its control stream and the HTTP
// exchanges the node opens toward it. It lives exactly as long as that session
// (epoch fencing: a reconnecting worker forms a new registration, and the old
// session dies with it).
//
// It carries no agent name, deployment or tenant identity: those name the Scope
// it was registered into, which is the embedder's to key.
type Registration struct {
	WorkerID string
	Manifest *Manifest

	draining func() bool

	lock    sync.RWMutex
	session Session
	closed  bool
}

// RegistrationParams is fixed for the life of the registration, which lasts
// exactly as long as the session.
type RegistrationParams struct {
	WorkerID string
	Manifest *Manifest

	// Session is the worker's live data-plane session: the WebTransport session
	// that also carries its control stream. One session per worker.
	Session Session
	// Draining reports that the worker is shedding; a shedding worker takes no
	// new streams.
	Draining func() bool
}

func NewRegistration(params RegistrationParams) *Registration {
	return &Registration{
		WorkerID: params.WorkerID,
		Manifest: params.Manifest,
		draining: params.Draining,
		session:  params.Session,
	}
}

// IsDraining is false when no drain signal was supplied.
func (r *Registration) IsDraining() bool {
	return r.draining != nil && r.draining()
}

func (r *Registration) getSession() Session {
	r.lock.RLock()
	defer r.lock.RUnlock()
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
	r.lock.Lock()
	if r.closed {
		r.lock.Unlock()
		return
	}
	r.closed = true
	s := r.session
	r.session = nil
	r.lock.Unlock()
	if s != nil {
		s.Close("registration closed")
	}
}

// Registry fences worker epochs on this node. Worker ids are server-issued and
// unique across the node, so this index is tenancy-blind: it exists only so a
// reconnecting worker supersedes its own stale epoch, wherever that epoch was
// scoped.
//
// It holds the outermost lock in this package: a Scope's lock may be taken while
// holding it, never the reverse.
type Registry struct {
	lock sync.RWMutex
	regs map[string]*regEntry // by worker id
}

// regEntry remembers which scope an epoch was registered into, so a supersede
// can retract it from there even when the worker came back under a different
// agent name or deployment.
type regEntry struct {
	reg   *Registration
	scope *Scope
}

func NewRegistry() *Registry {
	return &Registry{regs: make(map[string]*regEntry)}
}

// Register records a registration in a scope. A worker id already present is
// superseded: worker ids are stable across reconnects, and the retiring session
// must not be able to strand the new epoch (its own Deregister is a no-op once
// replaced). The superseded epoch's session is closed.
func (g *Registry) Register(scope *Scope, r *Registration) {
	g.lock.Lock()
	e := g.regs[r.WorkerID]
	var superseded, old *Registration
	if e != nil {
		superseded = e.reg
		if e.scope == scope {
			old = e.reg
		} else {
			// nothing pins agent name or deployment across epochs, so the
			// retiring epoch's routes may live in another scope
			e.scope.remove(e.reg)
		}
	}
	// one transaction, so an unchanged manifest keeps its Route pointers
	scope.replace(old, r)
	g.regs[r.WorkerID] = &regEntry{reg: r, scope: scope}
	g.lock.Unlock()
	if superseded != nil {
		superseded.close()
	}
}

// Deregister removes exactly this registration; it is a no-op when a newer epoch
// has already superseded it.
func (g *Registry) Deregister(r *Registration) {
	g.lock.Lock()
	e := g.regs[r.WorkerID]
	if e == nil || e.reg != r {
		g.lock.Unlock()
		return
	}
	delete(g.regs, r.WorkerID)
	e.scope.remove(r)
	g.lock.Unlock()
	r.close()
}
