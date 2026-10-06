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

package router

import "errors"

const MaxRoutes = 1 << 16

var errTooManyRoutes = errors.New("router: too many routes")

type buildNode[T any] struct {
	kids   []buildEdge[T]
	leaves []leaf[T]
	minIdx uint32
}

type buildEdge[T any] struct {
	lit    string
	to     *buildNode[T]
	minIdx uint32
	kind   kind
}

// Builder accumulates a route table. A route's index is its position in the
// order added.
type Builder[T any] struct {
	root      *buildNode[T]
	n         uint32
	ambiguous []string
}

func NewBuilder[T any]() *Builder[T] {
	return &Builder[T]{root: &buildNode[T]{minIdx: noIdx}}
}

// Add appends a route. Routes must be added in declaration order: the first
// one added that matches a path wins.
func (b *Builder[T]) Add(t *Template, m Mask, v T) error {
	if b.n >= MaxRoutes {
		return errTooManyRoutes
	}
	idx := b.n
	b.n++

	if t.Ambiguous() {
		b.ambiguous = append(b.ambiguous, t.raw)
	}

	n := b.root
	if n.minIdx == noIdx {
		n.minIdx = idx
	}
	for _, e := range t.elements {
		if e.kind == kindLiteral {
			n = insertLiteral(n, e.lit, idx)
		} else {
			n = insertParam(n, e.kind, idx)
		}
	}
	n.leaves = append(n.leaves, leaf[T]{idx: idx, mask: m, val: v})
	return nil
}

// Build freezes the table, compacting its nodes into one arena.
func (b *Builder[T]) Build() *Router[T] {
	r := &Router[T]{ambiguous: b.ambiguous, routes: int(b.n)}
	r.nodes = make([]node[T], 0, countNodes(b.root))
	r.compact(b.root)
	return r
}

func countNodes[T any](n *buildNode[T]) int {
	total := 1
	for i := range n.kids {
		total += countNodes(n.kids[i].to)
	}
	return total
}

func (r *Router[T]) compact(bn *buildNode[T]) uint32 {
	ni := uint32(len(r.nodes))
	r.nodes = append(r.nodes, node[T]{leaves: bn.leaves, minIdx: bn.minIdx})

	kids := make([]edge, len(bn.kids))
	for i := range bn.kids {
		be := &bn.kids[i]
		kids[i] = edge{lit: be.lit, minIdx: be.minIdx, kind: be.kind, single: singleRun(be)}
		if be.kind == kindLiteral {
			kids[i].first = be.lit[0]
		}
	}
	// recurse before wiring: appending children may move the arena
	for i := range bn.kids {
		kids[i].to = r.compact(bn.kids[i].to)
	}
	r.nodes[ni].kids = kids
	return ni
}

// singleRun reports that only the convertor's greedy run can lead anywhere: a
// shorter run leaves at the head a byte the convertor could have consumed, and
// no child below can start with one. A terminal param qualifies vacuously.
func singleRun[T any](e *buildEdge[T]) bool {
	if e.kind == kindLiteral || e.kind == kindUUID {
		return true
	}
	for i := range e.to.kids {
		k := &e.to.kids[i]
		if k.kind != kindLiteral || e.kind.charset(k.lit[0]) {
			return false
		}
	}
	return true
}

func insertLiteral[T any](n *buildNode[T], lit string, idx uint32) *buildNode[T] {
	for len(lit) > 0 {
		e := literalEdge(n, lit[0])
		if e == nil {
			child := &buildNode[T]{minIdx: idx}
			n.kids = append(n.kids, buildEdge[T]{kind: kindLiteral, lit: lit, to: child, minIdx: idx})
			return child
		}
		cp := commonPrefix(e.lit, lit)
		if cp < len(e.lit) {
			// the edge keeps its slot and its minIdx, so kids stay ordered
			mid := &buildNode[T]{minIdx: e.minIdx}
			mid.kids = append(mid.kids, buildEdge[T]{kind: kindLiteral, lit: e.lit[cp:], to: e.to, minIdx: e.minIdx})
			e.lit, e.to = e.lit[:cp], mid
		}
		n, lit = e.to, lit[cp:]
	}
	return n
}

func insertParam[T any](n *buildNode[T], k kind, idx uint32) *buildNode[T] {
	for i := range n.kids {
		if n.kids[i].kind == k {
			return n.kids[i].to
		}
	}
	child := &buildNode[T]{minIdx: idx}
	n.kids = append(n.kids, buildEdge[T]{kind: k, to: child, minIdx: idx})
	return child
}

// literalEdge finds the one literal edge that can consume c. Splitting keeps
// the first bytes of a node's literal edges distinct, so there is at most one.
func literalEdge[T any](n *buildNode[T], c byte) *buildEdge[T] {
	for i := range n.kids {
		if e := &n.kids[i]; e.kind == kindLiteral && e.lit[0] == c {
			return e
		}
	}
	return nil
}

func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}
