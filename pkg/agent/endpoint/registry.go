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

	"github.com/livekit/protocol/logger"
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

	draining func() bool

	lock    sync.RWMutex
	session Session
	closed  bool
}

// RegistrationParams is fixed for the life of the registration, which lasts
// exactly as long as the session.
type RegistrationParams struct {
	WorkerID   string
	APIKey     string
	AgentName  string
	Deployment string
	Manifest   *Manifest

	// Session is the worker's live data-plane session: the WebTransport session
	// that also carries its control stream. One session per worker.
	Session Session
	// Draining reports that the worker is shedding; a shedding worker takes no
	// new streams.
	Draining func() bool
}

func NewRegistration(params RegistrationParams) *Registration {
	return &Registration{
		WorkerID:   params.WorkerID,
		APIKey:     params.APIKey,
		AgentName:  params.AgentName,
		Deployment: params.Deployment,
		Manifest:   params.Manifest,
		draining:   params.Draining,
		session:    params.Session,
	}
}

func (r *Registration) key() regKey {
	return regKey{r.APIKey, r.AgentName, normalizeDeployment(r.Deployment)}
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

// Registry tracks data-plane registrations on this node, keyed by
// (api key, agent name, deployment). The api key is the project identity in OSS.
//
// Each key also owns a merged route table. Registrations and routes move
// together under g.lock, so a worker present in byKey always has its routes
// installed.
type Registry struct {
	logger logger.Logger

	lock   sync.RWMutex
	regs   map[string]*Registration // by worker id
	byKey  map[regKey][]*Registration
	tables map[regKey]*routeTable
}

type regKey struct {
	apiKey     string
	agentName  string
	deployment string
}

func NewRegistry() *Registry {
	return &Registry{
		logger: logger.GetLogger().WithComponent("agents.endpoint"),
		regs:   make(map[string]*Registration),
		byKey:  make(map[regKey][]*Registration),
		tables: make(map[regKey]*routeTable),
	}
}

// Register records a registration. A worker id already present is superseded:
// worker ids are stable across reconnects, and the retiring session must not be
// able to strand the new epoch (its own Deregister is a no-op once replaced).
// The superseded epoch's session is closed.
func (g *Registry) Register(r *Registration) {
	key := r.key()
	g.lock.Lock()
	superseded := g.regs[r.WorkerID]
	old := superseded
	if old != nil {
		g.unlinkLocked(old)
		if old.key() != key {
			// nothing pins agent name or deployment across epochs, so the
			// retiring epoch's routes may live in another table
			g.retractLocked(old)
			old = nil
		}
	}
	// one transaction, so an unchanged manifest keeps its Route pointers
	g.tableLocked(key).mutate(old, r)
	g.regs[r.WorkerID] = r
	g.byKey[key] = append(g.byKey[key], r)
	g.lock.Unlock()
	if superseded != nil {
		superseded.close()
	}
}

// tableLocked returns the key's route table, creating it on first use. Callers
// hold g.lock.
func (g *Registry) tableLocked(key regKey) *routeTable {
	tbl := g.tables[key]
	if tbl == nil {
		tbl = newRouteTable(key, g.logger)
		g.tables[key] = tbl
	}
	return tbl
}

// table returns a deployment's merged route table, or nil when no worker holds
// the key.
func (g *Registry) table(apiKey, agentName, deployment string) *routeTable {
	g.lock.RLock()
	defer g.lock.RUnlock()
	return g.tables[regKey{apiKey, agentName, normalizeDeployment(deployment)}]
}

// removeLocked unlinks a registration from all indexes. Callers hold g.lock.
func (g *Registry) removeLocked(r *Registration) {
	g.unlinkLocked(r)
	g.retractLocked(r)
}

// unlinkLocked drops a registration from the worker-id and key indexes.
func (g *Registry) unlinkLocked(r *Registration) {
	delete(g.regs, r.WorkerID)
	key := r.key()
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

// retractLocked drops a registration's routes. A table exists only while it
// holds routes.
func (g *Registry) retractLocked(r *Registration) {
	key := r.key()
	tbl := g.tables[key]
	if tbl == nil {
		return
	}
	tbl.mutate(r, nil)
	if tbl.empty() {
		delete(g.tables, key)
	}
}

// Deregister removes exactly this registration; it is a no-op when a newer
// epoch has already superseded it.
func (g *Registry) Deregister(r *Registration) {
	g.lock.Lock()
	if g.regs[r.WorkerID] != r {
		g.lock.Unlock()
		return
	}
	g.removeLocked(r)
	g.lock.Unlock()
	r.close()
}

// Candidates returns the registrations for (api key, agent name, deployment segment).
func (g *Registry) Candidates(apiKey, agentName, deployment string) []*Registration {
	g.lock.RLock()
	defer g.lock.RUnlock()
	return slices.Clone(g.byKey[regKey{apiKey, agentName, normalizeDeployment(deployment)}])
}

// SingleAPIKey returns the api key when every registration shares one - the OSS
// resolution for unauthenticated requests to public endpoints. ok is false when
// zero or multiple keys are present.
func (g *Registry) SingleAPIKey() (string, bool) {
	g.lock.RLock()
	defer g.lock.RUnlock()
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
