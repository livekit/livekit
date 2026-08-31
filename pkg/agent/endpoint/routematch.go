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
	"regexp"
	"strings"
)

// canonical wildcard tokens; the control-char prefix keeps them from colliding
// with any real path segment
const (
	tokStr   = "\x00s" // {x} or {x:str} - any single segment
	tokInt   = "\x00i" // {x:int}
	tokFloat = "\x00f" // {x:float}
	tokUUID  = "\x00u" // {x:uuid}
	tokGlob  = "\x00p" // {x:path} - greedy, spans the rest of the path

	segSep = "\x1f" // joins canonical segments into an index key

	// a route deeper than this is rejected at registration; it bounds both the
	// filter's prefix count and the walk's depth
	MaxRouteDepth = 32
	// a request longer than this skips the precise walk (falls back to
	// relay-and-let-the-worker-decide); real paths are a handful of segments
	maxMatchDepth = 64
)

var (
	intRe   = regexp.MustCompile(`^[0-9]+$`)
	floatRe = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)
)

// canonicalizeTemplate turns a starlette path template into canonical segments
// (params -> typed tokens, {:path} -> glob). A segment that mixes a literal
// with a param (e.g. "{name}.json") canonicalizes to the str token: the edge
// over-approximates the shape and the worker enforces the literal part.
func canonicalizeTemplate(path string) ([]string, error) {
	if _, err := ParseTemplate(path); err != nil { // validates '/', convertors, dup params
		return nil, err
	}
	segs := splitSegments(path)
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		tok, glob := canonicalSegment(seg)
		out = append(out, tok)
		if glob {
			// a path convertor spans slashes: the glob is terminal, and any
			// segments after it are dropped so the walk over-approximates (a
			// concrete request matching the pre-glob prefix always matches; the
			// worker's router enforces whatever follows). Truncating here can
			// only add matches, never drop one.
			break
		}
	}
	return out, nil
}

// canonicalSegment maps a template segment to its canonical token; glob is true
// when the segment contains a path convertor (spans slashes).
func canonicalSegment(seg string) (tok string, glob bool) {
	matches := paramRegex.FindAllStringSubmatchIndex(seg, -1)
	if len(matches) == 0 {
		return seg, false // pure literal
	}
	// any path convertor in the segment makes it a glob, whether it is the whole
	// segment ("{rest:path}") or mixed with literal text ("pre{rest:path}") -
	// str would be strictly narrower than the glob's `.*` and would drop matches
	for _, m := range matches {
		if m[4] != -1 && seg[m[4]+1:m[5]] == "path" {
			return tokGlob, true
		}
	}
	// a clean whole-segment param: "{name}" or "{name:conv}"
	if m := matches[0]; len(matches) == 1 && m[0] == 0 && m[1] == len(seg) {
		conv := "str"
		if m[4] != -1 {
			conv = seg[m[4]+1 : m[5]]
		}
		switch conv {
		case "int":
			return tokInt, false
		case "float":
			return tokFloat, false
		case "uuid":
			return tokUUID, false
		default: // str and anything ParseTemplate already accepted
			return tokStr, false
		}
	}
	// a non-path param mixed with literal text: over-approximate to str (str's
	// [^/]+ is wider than any typed single-segment convertor, so no false negative)
	return tokStr, false
}

// candidateTokens returns the canonical tokens a concrete request segment could
// match: the literal itself, the str wildcard (always), and any typed wildcard
// whose pattern the value satisfies.
func candidateTokens(seg string) []string {
	toks := make([]string, 0, 5)
	toks = append(toks, seg, tokStr)
	if intRe.MatchString(seg) {
		toks = append(toks, tokInt)
	}
	if floatRe.MatchString(seg) {
		toks = append(toks, tokFloat)
	}
	if uuidRe.MatchString(seg) {
		toks = append(toks, tokUUID)
	}
	return toks
}

func splitSegments(path string) []string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/") // /x/ and /x match the same shape; worker 307s
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func extendKey(prefix, tok string) string {
	if prefix == "" {
		return tok
	}
	return prefix + segSep + tok
}

// RouteKey qualifies a canonical path terminal with its HTTP method, so a
// terminal is a route only for the method(s) the worker declared. Methods are
// uppercase ASCII (allowlisted at registration) and canonical tokens start with
// a control char, so method and path can't collide. Prefix keys stay path-only
// (the pruned walk is method-agnostic until the terminal check).
func RouteKey(method, canonicalRoute string) string {
	return method + segSep + canonicalRoute
}

// PrefixIndex is the backing an edge matches against: whether a canonical prefix
// key is a prefix of some route, and whether it is a complete route. An exact
// map-backed index (ExactIndex) gives precise answers; a cuckoo-filter backing
// trades a bounded false-positive rate for a compact, replicable form.
type PrefixIndex interface {
	HasPrefix(key string) bool
	IsRoute(key string) bool
}

// Matches reports whether the request (method + path) matches any route in the
// index, via the pruned walk. The path walk is method-agnostic (prefix keys are
// path-only); only the terminal check is method-qualified. A path deeper than
// maxMatchDepth returns true so the caller relays and lets the worker decide
// (never a spurious 404 on a deep path).
func Matches(idx PrefixIndex, method, path string) bool {
	segs := splitSegments(path)
	if len(segs) > maxMatchDepth {
		return true
	}

	live := map[string]struct{}{"": {}}
	for _, seg := range segs {
		// a glob at any currently-live prefix eats this segment and the rest
		for p := range live {
			if idx.IsRoute(RouteKey(method, extendKey(p, tokGlob))) {
				return true
			}
		}
		next := make(map[string]struct{})
		for p := range live {
			for _, tok := range candidateTokens(seg) {
				k := extendKey(p, tok)
				if idx.HasPrefix(k) {
					next[k] = struct{}{}
				}
			}
		}
		if len(next) == 0 {
			return false
		}
		live = next
	}
	// consumed every segment: a live prefix that is a complete route for this
	// method matches, and a glob at a live prefix matches the empty remainder too
	for p := range live {
		if idx.IsRoute(RouteKey(method, p)) || idx.IsRoute(RouteKey(method, extendKey(p, tokGlob))) {
			return true
		}
	}
	return false
}

// ExactIndex is a precise, map-backed PrefixIndex built from a set of route
// templates. Used where the full route set is available locally (a single-node
// server, or a test); the cuckoo-filter backing is used where the set must be
// replicated compactly.
type ExactIndex struct {
	prefixes map[string]struct{}
	routes   map[string]struct{}
}

func NewExactIndex() *ExactIndex {
	return &ExactIndex{
		prefixes: make(map[string]struct{}),
		routes:   make(map[string]struct{}),
	}
}

// Add canonicalizes a template and inserts its path prefixes plus a
// method-qualified terminal per declared method.
func (x *ExactIndex) Add(methods []string, path string) error {
	segs, err := canonicalizeTemplate(path)
	if err != nil {
		return err
	}
	key := ""
	for _, s := range segs {
		key = extendKey(key, s)
		x.prefixes[key] = struct{}{}
	}
	for _, m := range methods {
		x.routes[RouteKey(m, key)] = struct{}{}
	}
	return nil
}

func (x *ExactIndex) HasPrefix(key string) bool {
	_, ok := x.prefixes[key]
	return ok
}

func (x *ExactIndex) IsRoute(key string) bool {
	_, ok := x.routes[key]
	return ok
}

// CanonicalKeys returns the index keys for a route template: every prefix key
// (including the full-length one) and the terminal route key. A multi-node
// layer uses these to populate a compact backing (e.g. a cuckoo filter) that it
// replicates, then matches with Matches against a FilterIndex over it.
func CanonicalKeys(path string) (prefixes []string, route string, err error) {
	segs, err := canonicalizeTemplate(path)
	if err != nil {
		return nil, "", err
	}
	key := ""
	prefixes = make([]string, 0, len(segs))
	for _, s := range segs {
		key = extendKey(key, s)
		prefixes = append(prefixes, key)
	}
	return prefixes, key, nil
}

// RouteDepth reports the canonical segment count of a template, for enforcing
// MaxRouteDepth at registration.
func RouteDepth(path string) (int, error) {
	segs, err := canonicalizeTemplate(path)
	if err != nil {
		return 0, err
	}
	return len(segs), nil
}
