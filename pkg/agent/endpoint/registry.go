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
	"crypto/subtle"
	"errors"
	"slices"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
)

// DefaultDeployment is the URL segment that addresses workers registered with an
// empty deployment name (self-hosted workers typically set none).
const DefaultDeployment = "default"

var (
	ErrAttachRejected = errors.New("attach rejected")
	ErrUnknownWorker  = errors.New("unknown worker registration")
	// ErrWrongEpoch means a registration exists for the worker but the wire's
	// credentials name a different epoch: a multi-node adopter re-validates
	// against the registration holder and refreshes, since the holder is the
	// authority on the current epoch.
	ErrWrongEpoch = errors.New("wrong registration epoch")
)

func normalizeDeployment(d string) string {
	if d == "" {
		return DefaultDeployment
	}
	return d
}

// Registration is one worker's data-plane state: its manifest, negotiated
// settings, and attached data connections. It lives exactly
// as long as the worker's control connection (epoch fencing: a reconnecting
// worker forms a new registration with a fresh attach token; connections of the
// old epoch die with it).
type Registration struct {
	WorkerID   string
	InstanceID string
	APIKey     string
	Deployment string
	Manifest   *Manifest
	// Endpoints is the raw manifest as declared, kept so a multi-node layer can
	// replicate the route set for remote path matching without re-deriving it.
	Endpoints []*livekit.AgentHttp_AgentEndpoint
	Settings  Settings
	Logger    logger.Logger

	// Load and Draining are provided by the control-plane layer that owns the
	// worker (reported load rides UpdateWorkerStatus on the control connection).
	Load     func() float32
	Draining func() bool

	// pendingAttaches counts slots reserved by validated-but-unadopted wires so
	// two racing attaches at the cap cannot both be acked
	pendingAttaches int

	mu     sync.Mutex
	conns  []*DataConn
	closed bool
}

func (r *Registration) validateAttach(instanceID, token string) error {
	if subtle.ConstantTimeCompare([]byte(token), []byte(r.Settings.AttachToken)) != 1 {
		return ErrAttachRejected
	}
	if instanceID != r.InstanceID {
		return ErrAttachRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrAttachRejected
	}
	if len(r.conns) >= int(r.Settings.DataConnCount) {
		return ErrAttachRejected
	}
	return nil
}

// beginAttach validates the wire's credentials and atomically reserves a pool
// slot, so the attach ack can be written before adoption without two racing
// wires both being acked at the cap.
func (r *Registration) beginAttach(instanceID, token string) (*AttachTicket, error) {
	if subtle.ConstantTimeCompare([]byte(token), []byte(r.Settings.AttachToken)) != 1 ||
		instanceID != r.InstanceID {
		return nil, ErrWrongEpoch
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrAttachRejected
	}
	// the pool is fixed size: tolerate redials, but never more live conns than
	// negotiated
	if len(r.conns)+r.pendingAttaches >= int(r.Settings.DataConnCount) {
		return nil, ErrAttachRejected
	}
	r.pendingAttaches++
	return &AttachTicket{r: r}, nil
}

// AttachTicket is a reserved pool slot: Complete adopts the wire into it, Abort
// releases it.
type AttachTicket struct {
	r    *Registration
	used bool
}

// Complete adopts the wire; it reports false when the registration closed while
// the ack was in flight (the caller must close the wire).
func (t *AttachTicket) Complete(wire WireConn, params WireParams) bool {
	r := t.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.used {
		return false
	}
	t.used = true
	r.pendingAttaches--
	if r.closed {
		return false
	}
	conn := NewDataConn(wire, params, r.removeConn, r.Logger)
	r.conns = append(r.conns, conn)
	return true
}

func (t *AttachTicket) Abort() {
	r := t.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.used {
		return
	}
	t.used = true
	r.pendingAttaches--
}

func (r *Registration) removeConn(c *DataConn) {
	r.mu.Lock()
	if i := slices.Index(r.conns, c); i != -1 {
		r.conns = slices.Delete(r.conns, i, i+1)
	}
	r.mu.Unlock()
}

// PickConn places a new stream: the lightest non-heavy connection with capacity,
// falling back to spread-evenly when everything is heavy.
func (r *Registration) PickConn() *DataConn {
	r.mu.Lock()
	conns := slices.Clone(r.conns)
	r.mu.Unlock()

	var best, bestHeavy *DataConn
	var bestScore, bestHeavyScore int64
	for _, c := range conns {
		if !c.HasCapacity() {
			continue
		}
		score, heavy := c.Score()
		if heavy {
			if bestHeavy == nil || score < bestHeavyScore {
				bestHeavy, bestHeavyScore = c, score
			}
			continue
		}
		if best == nil || score < bestScore {
			best, bestScore = c, score
		}
	}
	if best != nil {
		return best
	}
	return bestHeavy
}

// AttachedConns reports live data connections.
func (r *Registration) AttachedConns() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

func (r *Registration) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	conns := slices.Clone(r.conns)
	r.conns = nil
	r.mu.Unlock()

	for _, c := range conns {
		c.Close(ErrConnClosed)
	}
}

// Registry tracks data-plane registrations on this node, keyed by
// (api key, deployment). The api key is the project identity in OSS.
type Registry struct {
	mu    sync.Mutex
	regs  map[string]*Registration // by worker id
	byKey map[regKey][]*Registration

	// scope hooks fire when a scope (api key / project) gains its first or
	// loses its last registration; the Remote uses them to (de)register the
	// resolve topic on the bus
	scopeCounts   map[string]int
	onScopeActive func(scope string)
	onScopeIdle   func(scope string)

	// hookMu serializes scope hook invocations. Transitions are decided against
	// the registry's CURRENT state under hookMu, never from values computed
	// earlier: a reconnect's activation racing the old connection's idle must
	// not leave a live scope without its resolve topic.
	hookMu        sync.Mutex
	scopeNotified map[string]bool
}

type regKey struct {
	apiKey     string
	deployment string
}

func NewRegistry() *Registry {
	return &Registry{
		regs:          make(map[string]*Registration),
		byKey:         make(map[regKey][]*Registration),
		scopeCounts:   make(map[string]int),
		scopeNotified: make(map[string]bool),
	}
}

// notifyScope reconciles the scope's hook state with the registry's current
// truth. Racing register/deregister notifications converge instead of
// interleaving.
func (g *Registry) notifyScope(scope string) {
	g.hookMu.Lock()
	defer g.hookMu.Unlock()
	g.mu.Lock()
	active := g.scopeCounts[scope] > 0
	activeHook, idleHook := g.onScopeActive, g.onScopeIdle
	g.mu.Unlock()
	if activeHook == nil && idleHook == nil {
		return
	}
	if g.scopeNotified[scope] == active {
		return
	}
	if active {
		g.scopeNotified[scope] = true
		activeHook(scope)
	} else {
		delete(g.scopeNotified, scope)
		idleHook(scope)
	}
}

// SetScopeHooks installs the scope activation callbacks, replaying currently
// active scopes so a resolver layered on after registrations still registers
// its topics. Multi-node deployments use this to answer endpoint resolves only
// while they hold registrations for a scope.
func (g *Registry) SetScopeHooks(active, idle func(scope string)) {
	g.hookMu.Lock()
	defer g.hookMu.Unlock()
	g.mu.Lock()
	g.onScopeActive = active
	g.onScopeIdle = idle
	scopes := make([]string, 0, len(g.scopeCounts))
	for scope, n := range g.scopeCounts {
		if n > 0 {
			scopes = append(scopes, scope)
		}
	}
	g.mu.Unlock()
	if active != nil {
		for _, s := range scopes {
			if !g.scopeNotified[s] {
				g.scopeNotified[s] = true
				active(s)
			}
		}
	}
}

// NewAttachToken mints a registration's attach token.
func NewAttachToken() string {
	return guid.New("ATT_")
}

// Register adopts a registration. A worker id already present is superseded:
// worker ids are stable across reconnects, and the retiring control connection
// must not be able to strand the new epoch (its own Deregister is a no-op once
// replaced). The superseded epoch's connections die with it.
func (g *Registry) Register(r *Registration) error {
	key := regKey{r.APIKey, normalizeDeployment(r.Deployment)}
	g.mu.Lock()
	old := g.regs[r.WorkerID]
	if old != nil {
		g.removeLocked(old)
	}
	g.regs[r.WorkerID] = r
	g.byKey[key] = append(g.byKey[key], r)
	g.scopeCounts[r.APIKey]++
	g.mu.Unlock()
	g.notifyScope(r.APIKey)
	if old != nil {
		if old.APIKey != r.APIKey {
			g.notifyScope(old.APIKey)
		}
		old.close()
	}
	return nil
}

// removeLocked unlinks a registration from all indexes. Callers hold g.mu.
func (g *Registry) removeLocked(r *Registration) {
	delete(g.regs, r.WorkerID)
	key := regKey{r.APIKey, normalizeDeployment(r.Deployment)}
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
	g.scopeCounts[r.APIKey]--
	if g.scopeCounts[r.APIKey] == 0 {
		delete(g.scopeCounts, r.APIKey)
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
	g.notifyScope(r.APIKey)
	r.close()
}

// ValidateAttach checks an attach without adopting the connection, so the
// attach response can be written before stream frames may flow.
func (g *Registry) ValidateAttach(workerID, instanceID, token string) error {
	g.mu.Lock()
	r, ok := g.regs[workerID]
	g.mu.Unlock()
	if !ok {
		return ErrUnknownWorker
	}
	return r.validateAttach(instanceID, token)
}

// BeginAttach validates an attach and reserves a pool slot; the caller writes
// the ack and then Completes (or Aborts) the ticket.
func (g *Registry) BeginAttach(workerID, instanceID, token string) (*AttachTicket, error) {
	g.mu.Lock()
	r, ok := g.regs[workerID]
	g.mu.Unlock()
	if !ok {
		return nil, ErrUnknownWorker
	}
	return r.beginAttach(instanceID, token)
}

// Candidates returns the registrations for (api key, deployment segment).
func (g *Registry) Candidates(apiKey, deployment string) []*Registration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.byKey[regKey{apiKey, normalizeDeployment(deployment)}])
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
