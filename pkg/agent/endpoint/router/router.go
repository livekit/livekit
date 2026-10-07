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

// Package router matches request paths against an ordered table of
// starlette-style path templates. The first route declared that matches wins:
// declaration order is the entire priority rule.
package router

import (
	"go.uber.org/zap/zapcore"
)

// maxSteps bounds the backtracking a single Match may do. Only a non-single
// edge spends a step, so a table of unambiguous templates never reaches it.
const maxSteps = 100_000

// Mask is an opaque per-route tag. A route fully matches when its mask
// intersects the query mask, and partially matches when only its template does.
type Mask uint32

type Result uint8

const (
	ResultNone Result = iota
	// ResultPartial means a template matched but no route carrying the queried
	// mask did.
	ResultPartial
	ResultFull
	// ResultOverBudget means the search exceeded maxSteps and was abandoned, so
	// no route was decided.
	ResultOverBudget
)

func (r Result) String() string {
	switch r {
	case ResultNone:
		return "none"
	case ResultPartial:
		return "partial"
	case ResultFull:
		return "full"
	case ResultOverBudget:
		return "over-budget"
	}
	return "unknown"
}

// noIdx is above every route index, so an unset best never prunes.
const noIdx = ^uint32(0)

type edge struct {
	lit    string // kindLiteral only
	to     uint32 // index into Router.nodes
	minIdx uint32 // mirrors nodes[to].minIdx, so a prune dereferences no child
	kind   kind
	first  byte // kindLiteral only: lit[0]
	single bool // the convertor admits one viable run at any position
}

type leaf[T any] struct {
	idx  uint32
	mask Mask
	val  T
}

type node[T any] struct {
	kids []edge
	// several routes can terminate at one node: /u/{id} and /u/{name} compile
	// alike, and a path may be declared once per method
	leaves []leaf[T]
	minIdx uint32
}

// Router matches paths against a route table. It is built once and never
// mutated, so readers need no lock and a new table is published by a single
// atomic.Pointer store, which synchronises everything a reader reaches.
type Router[T any] struct {
	nodes     []node[T]
	ambiguous []string
	routes    int
}

// MarshalLogObject describes the table's shape. A Router holds no per-match
// state.
func (r *Router[T]) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}
	e.AddInt("routes", r.routes)
	if len(r.ambiguous) > 0 {
		err := e.AddArray("ambiguous", zapcore.ArrayMarshalerFunc(func(a zapcore.ArrayEncoder) error {
			for _, t := range r.ambiguous {
				a.AppendString(t)
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	return nil
}

// Ambiguous returns the templates whose shape forces the matcher to backtrack -
// a param that a following literal can extend, adjacent params, or a non-final
// path convertor. The result is read-only.
func (r *Router[T]) Ambiguous() []string { return r.ambiguous }
