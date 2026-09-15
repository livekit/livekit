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

import "strings"

// state lives in Match's frame and is threaded by pointer; nothing reachable
// from it may escape.
type state[T any] struct {
	path string
	q    Mask
	val  T
	// best is the lowest index of a route matching both path and mask. Only a
	// full match may assign it, since it gates pruning.
	best uint32
	// acc is the second accepting position, len(path)-1 when the path ends in a
	// newline and -1 otherwise: a template anchors as `\n?$`.
	acc     int
	steps   int
	partial bool
}

// Match returns the value of the first route in declaration order whose
// template matches path and whose mask intersects q. It returns ResultPartial
// when a template matched but no such route carried the mask, and
// ResultOverBudget when the search exceeded its step budget.
//
// Match allocates nothing.
func (r *Router[T]) Match(path string, q Mask) (T, Result) {
	st := state[T]{path: path, q: q, best: noIdx, acc: -1}
	if n := len(path); n > 0 && path[n-1] == '\n' {
		st.acc = n - 1
	}

	if !r.walk(0, 0, &st) {
		// a cut-short search may have missed a lower-indexed route, so anything
		// found is not necessarily what declaration order selects
		var zero T
		return zero, ResultOverBudget
	}
	switch {
	case st.best != noIdx:
		return st.val, ResultFull
	case st.partial:
		var zero T
		return zero, ResultPartial
	default:
		var zero T
		return zero, ResultNone
	}
}

// walk explores the subtree at ni with pos bytes of the path consumed,
// reporting false when the step budget ran out. Declaration order is
// uncorrelated with depth, so a match is not a stopping condition; only the
// minIdx prune cuts the scan short.
func (r *Router[T]) walk(ni uint32, pos int, st *state[T]) bool {
	n := &r.nodes[ni]

	if pos == len(st.path) || pos == st.acc {
		for i := range n.leaves {
			lf := &n.leaves[i]
			if lf.idx >= st.best {
				break
			}
			if lf.mask&st.q != 0 {
				st.best, st.val = lf.idx, lf.val
				break
			}
			st.partial = true
		}
		// a path convertor matches the empty string, so a route may still
		// terminate below this node at this same position
	}

	rest := st.path[pos:]
	for i := range n.kids {
		e := &n.kids[i]
		if e.minIdx >= st.best {
			break // kids ascend by minIdx, so no later one can improve on best
		}

		if e.kind == kindLiteral {
			if len(e.lit) > len(rest) || rest[0] != e.first || !strings.HasPrefix(rest, e.lit) {
				continue
			}
			if !r.walk(e.to, pos+len(e.lit), st) {
				return false
			}
			continue
		}

		k := e.kind.scan(rest)
		if k < 0 {
			continue
		}
		if e.single {
			if !r.walk(e.to, pos+k, st) {
				return false
			}
			continue
		}
		for ; k >= 0; k = e.kind.next(rest, k) {
			st.steps++
			if st.steps > maxSteps {
				return false
			}
			if !r.walk(e.to, pos+k, st) {
				return false
			}
			if e.minIdx >= st.best {
				break
			}
		}
	}
	return true
}
