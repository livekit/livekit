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
	"cmp"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/router"
)

// routeKey identifies one route: the template shape it matches and the single
// method it serves. Templates differing only in param names share a shape, so
// /x/{a} and /x/{b} are one route.
type routeKey struct {
	canonical string
	method    router.Mask
}

// Route is one endpoint of a deployment and every worker that declared it. The
// compiled table holds these by pointer: a route outlives the tables built
// around it, and its worker set changes without a rebuild.
type Route struct {
	Template *router.Template
	Mask     router.Mask

	key routeKey
	// ord mirrors the merge position the published table was built from
	ord uint32

	lock    sync.RWMutex
	workers []routeWorker
}

// routeWorker is one worker's declaration of a route. Declarers of one route can
// disagree about visibility, position and spelling.
type routeWorker struct {
	reg    *Registration
	raw    string // the template as this worker declared it
	public bool
	idx    uint32 // its position in this worker's manifest
}

// declare records one worker's claim. A registration appears at most once, and
// callers walk endpoints in declaration order, so a path and method named twice
// keeps its first position.
func (r *Route) declare(w routeWorker) {
	r.lock.Lock()
	defer r.lock.Unlock()
	for i := range r.workers {
		if r.workers[i].reg == w.reg {
			return
		}
	}
	r.workers = append(r.workers, w)
}

// retract drops a registration's claim. Matching is by pointer: a supersede has
// both epochs live at once under one worker id.
func (r *Route) retract(reg *Registration) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.workers = slices.DeleteFunc(r.workers, func(w routeWorker) bool { return w.reg == reg })
}

// position reports the route's merge position - the lowest index it holds in any
// declaring manifest - and whether anything still declares it.
func (r *Route) position() (ord uint32, live bool) {
	r.lock.RLock()
	defer r.lock.RUnlock()
	ord = noOrder
	for _, w := range r.workers {
		ord = min(ord, w.idx)
	}
	return ord, len(r.workers) > 0
}

const noOrder = ^uint32(0)

// eligible copies out the workers that may serve the request. Nothing here may
// reach into a Registration: this lock must not be held across the transport.
func (r *Route) eligible(granted bool) (workers []routeWorker, denied bool) {
	r.lock.RLock()
	defer r.lock.RUnlock()
	for _, w := range r.workers {
		if !granted && !w.public {
			denied = true
			continue
		}
		workers = append(workers, w)
	}
	return
}

// routeTable is a deployment's merged route table. The tree is rebuilt only when
// the route set or its order changes, and is published by a single atomic store
// under lock.
type routeTable struct {
	logger logger.Logger

	lock   sync.Mutex
	routes map[routeKey]*Route
	tree   atomic.Pointer[router.Router[*Route]]
}

// newRouteTable takes the scope's logger as-is: whatever identifies the scope is
// curried in by the embedder, so the table holds no key of its own.
func newRouteTable(l logger.Logger) *routeTable {
	return &routeTable{
		logger: l,
		routes: make(map[routeKey]*Route),
	}
}

// mutate applies a removal and an addition as one transaction, republishing the
// tree if the merged table moved. Routes the removal empties are deleted only
// once the addition has run, so a registration replaced by an equivalent one
// keeps its Route pointers and the published tree stays live.
func (t *routeTable) mutate(remove, add *Registration) {
	t.lock.Lock()
	defer t.lock.Unlock()

	touched := make(map[*Route]struct{})
	if remove != nil {
		t.retractLocked(remove, touched)
	}
	if add != nil {
		t.declareLocked(add, touched)
	}

	dirty := false
	for r := range touched {
		ord, live := r.position()
		if !live {
			delete(t.routes, r.key)
			dirty = true
			continue
		}
		// a departure can raise a route's position and an arrival lower it;
		// either reorders the table
		if ord != r.ord {
			dirty = true
		}
	}
	if dirty {
		t.publishLocked()
	}
}

func (t *routeTable) declareLocked(reg *Registration, touched map[*Route]struct{}) {
	if reg.Manifest == nil {
		return
	}
	for i, ep := range reg.Manifest.Endpoints {
		for mask := ep.Mask; mask != 0; mask &= mask - 1 {
			key := routeKey{canonical: ep.Template.Canonical(), method: mask & -mask}
			r := t.routes[key]
			if r == nil {
				r = &Route{Template: ep.Template, Mask: key.method, key: key, ord: noOrder}
				t.routes[key] = r
			}
			r.declare(routeWorker{reg: reg, raw: ep.Template.String(), public: ep.Public, idx: uint32(i)})
			touched[r] = struct{}{}
		}
	}
}

func (t *routeTable) retractLocked(reg *Registration, touched map[*Route]struct{}) {
	for _, r := range t.routes {
		r.retract(reg)
		touched[r] = struct{}{}
	}
}

// publishLocked compiles the merged table and swaps it in. The builder assigns
// each route an index from its position in the order added, so routes must be
// added in merge order.
func (t *routeTable) publishLocked() {
	ordered := make([]*Route, 0, len(t.routes))
	for _, r := range t.routes {
		ordered = append(ordered, r)
	}
	for _, r := range ordered {
		r.ord, _ = r.position()
	}
	// routes sharing a position are separated by the tie-break alone, so the
	// comparison must be total and reach no further than the key
	slices.SortFunc(ordered, func(a, b *Route) int {
		if c := cmp.Compare(a.ord, b.ord); c != 0 {
			return c
		}
		if c := cmp.Compare(a.key.canonical, b.key.canonical); c != 0 {
			return c
		}
		return cmp.Compare(a.key.method, b.key.method)
	})

	b := router.NewBuilder[*Route]()
	for i, r := range ordered {
		if err := b.Add(r.Template, r.Mask, r); err != nil {
			// MaxManifestRoutes caps a worker, not the merged table; what
			// survives is the lowest stretch of the merge order
			t.logger.Warnw("agent endpoint route table truncated", err,
				"routes", len(ordered), "installed", i)
			break
		}
	}
	t.tree.Store(b.Build())
}

func (t *routeTable) empty() bool {
	t.lock.Lock()
	defer t.lock.Unlock()
	return len(t.routes) == 0
}

// match resolves a path against the merged table. matched is the dispatch test:
// a route reached through a tree loaded before its last worker left resolves to
// an empty set.
func (t *routeTable) match(path string, mask router.Mask, granted bool) (matched []routeWorker, route *Route, res router.Result, denied bool) {
	tree := t.tree.Load()
	if tree == nil {
		return nil, nil, router.ResultNone, false
	}
	rt, res := tree.Match(path, mask)
	if res != router.ResultFull {
		return nil, nil, res, false
	}
	matched, denied = rt.eligible(granted)
	return matched, rt, res, denied
}

// serves reports whether the table has a route for this exact path and method.
func (t *routeTable) serves(path string, mask router.Mask) bool {
	tree := t.tree.Load()
	if tree == nil {
		return false
	}
	_, res := tree.Match(path, mask)
	return res == router.ResultFull
}
