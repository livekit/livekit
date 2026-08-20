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
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"

	"github.com/livekit/protocol/livekit"
)

const MaxManifestRoutes = 256

// Route is one validated manifest entry.
type Route struct {
	Template *Template
	Methods  []string // uppercase; empty for websocket routes
	Kind     livekit.AgentHttp_AgentEndpointKind
	Public   bool
}

// Manifest is a worker's ordered route table.
type Manifest struct {
	routes []Route
}

// Version is a stable fnv64a digest of the route set, used to detect manifest
// skew across nodes serving the same deployment (all-agree => safe to route
// without inspecting the path). It is order-independent: two workers on the
// same revision hash identically regardless of route declaration order.
func (m *Manifest) Version() uint64 {
	lines := make([]string, 0, len(m.routes))
	for i := range m.routes {
		r := &m.routes[i]
		methods := append([]string(nil), r.Methods...)
		slices.Sort(methods)
		lines = append(lines, strconv.Itoa(int(r.Kind))+" "+r.Template.String()+" "+strings.Join(methods, ",")+" "+strconv.FormatBool(r.Public))
	}
	slices.Sort(lines)
	h := fnv.New64a()
	for _, l := range lines {
		_, _ = h.Write([]byte(l))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// MatchResult mirrors starlette's Match enum: a FULL match selects the route, a
// PARTIAL match (path matched, method didn't) yields 405 only after the whole
// table has been scanned, so a later route with the right method still wins.
type MatchResult int

const (
	MatchNone MatchResult = iota
	MatchPartial
	MatchFull
)

// ParseManifest validates a registration's endpoint list.
func ParseManifest(endpoints []*livekit.AgentHttp_AgentEndpoint) (*Manifest, error) {
	if len(endpoints) > MaxManifestRoutes {
		return nil, fmt.Errorf("manifest exceeds %d routes", MaxManifestRoutes)
	}
	m := &Manifest{routes: make([]Route, 0, len(endpoints))}
	for _, ep := range endpoints {
		tpl, err := ParseTemplate(ep.GetPath())
		if err != nil {
			return nil, err
		}
		var methods []string
		switch ep.GetKind() {
		case livekit.AgentHttp_AEK_HTTP:
			if len(ep.GetMethods()) == 0 {
				return nil, fmt.Errorf("endpoint %q declares no methods", ep.GetPath())
			}
			for _, method := range ep.GetMethods() {
				u := strings.ToUpper(method)
				if u != method {
					return nil, fmt.Errorf("endpoint %q method %q must be uppercase", ep.GetPath(), method)
				}
				methods = append(methods, u)
			}
		case livekit.AgentHttp_AEK_WEBSOCKET:
			if len(ep.GetMethods()) != 0 {
				return nil, fmt.Errorf("websocket endpoint %q must not declare methods", ep.GetPath())
			}
		default:
			return nil, fmt.Errorf("endpoint %q has unsupported kind %s", ep.GetPath(), ep.GetKind())
		}
		m.routes = append(m.routes, Route{
			Template: tpl,
			Methods:  methods,
			Kind:     ep.GetKind(),
			Public:   ep.GetPublic(),
		})
	}
	return m, nil
}

// Match resolves a request path against the table. websocket selects the
// websocket route class (upgrade requests); otherwise the HTTP class with
// method matching.
func (m *Manifest) Match(path, method string, websocket bool) (*Route, MatchResult) {
	partial := false
	for i := range m.routes {
		r := &m.routes[i]
		if websocket != (r.Kind == livekit.AgentHttp_AEK_WEBSOCKET) {
			continue
		}
		if !r.Template.Match(path) {
			continue
		}
		if websocket || slices.Contains(r.Methods, method) {
			return r, MatchFull
		}
		partial = true
	}
	if partial {
		return nil, MatchPartial
	}
	return nil, MatchNone
}

// RedirectSlashes reports whether the alternate-slash form of path would match,
// mirroring starlette's redirect_slashes default (FastAPI 307s /x/ to /x and
// vice versa when only the alternate form matches).
func (m *Manifest) RedirectSlashes(path, method string, websocket bool) (string, bool) {
	if path == "/" {
		return "", false
	}
	var alt string
	if strings.HasSuffix(path, "/") {
		alt = strings.TrimSuffix(path, "/")
	} else {
		alt = path + "/"
	}
	if _, res := m.Match(alt, method, websocket); res == MatchFull {
		return alt, true
	}
	return "", false
}
