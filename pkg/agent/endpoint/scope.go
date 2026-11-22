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
	"slices"
	"sync"

	"github.com/livekit/protocol/logger"
)

// Scope is one deployment's serving state on this node: the registrations that
// hold it and their merged route table. Who a scope belongs to is the embedder's
// business - nothing here stores a tenancy identity, and the embedder owns the
// map that scopes it.
//
// Registrations and routes move together under lock, so a worker present in the
// candidate set always has its routes installed.
type Scope struct {
	logger logger.Logger

	lock  sync.RWMutex
	regs  []*Registration
	table *routeTable // nil while nothing declares a route
}

// NewScope takes a logger the embedder has already curried with whatever
// identifies the scope; the package adds no identity of its own.
func NewScope(l logger.Logger) *Scope {
	return &Scope{logger: l}
}

// Candidates returns the registrations holding this scope. A nil scope holds
// none, so a caller that could not resolve one needs no separate test.
func (s *Scope) Candidates() []*Registration {
	if s == nil {
		return nil
	}
	s.lock.RLock()
	defer s.lock.RUnlock()
	return slices.Clone(s.regs)
}

// Empty reports that no registration holds this scope, so the embedder may drop
// it.
func (s *Scope) Empty() bool {
	if s == nil {
		return true
	}
	s.lock.RLock()
	defer s.lock.RUnlock()
	return len(s.regs) == 0
}

// routeTable returns the merged table, or nil when nothing declares a route. A
// scope whose last worker left must report nil rather than an empty tree: the
// front maps a missing table to "no worker for this deployment" (503) and an
// empty one to "no such path" (404).
func (s *Scope) routeTable() *routeTable {
	if s == nil {
		return nil
	}
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.table
}

// replace swaps one registration for another as a single transaction. Routes the
// removal empties are dropped only once the addition has run, so a registration
// replaced by an equivalent one keeps its Route pointers and the published tree
// stays live. Either side may be nil.
func (s *Scope) replace(remove, add *Registration) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if remove != nil {
		s.unlinkLocked(remove)
	}
	if add != nil {
		s.regs = append(s.regs, add)
	}
	s.mutateLocked(remove, add)
}

// remove drops a registration and its routes.
func (s *Scope) remove(r *Registration) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.unlinkLocked(r)
	s.mutateLocked(r, nil)
}

func (s *Scope) unlinkLocked(r *Registration) {
	if i := slices.Index(s.regs, r); i != -1 {
		s.regs = slices.Delete(s.regs, i, i+1)
	}
}

// mutateLocked applies the route change, creating the table on first use and
// dropping it once it empties.
func (s *Scope) mutateLocked(remove, add *Registration) {
	if s.table == nil {
		if add == nil {
			return
		}
		s.table = newRouteTable(s.logger)
	}
	s.table.mutate(remove, add)
	if s.table.empty() {
		s.table = nil
	}
}
