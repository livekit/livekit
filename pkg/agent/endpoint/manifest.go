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
	"net/http"
	"slices"
	"strings"

	"github.com/livekit/protocol/livekit"
)

const MaxManifestRoutes = 256

// allowedMethods is the set of HTTP methods a route may declare: exactly the
// verbs FastAPI (starlette) can route. Registration rejects anything else, so a
// typo ("GTE") or a garbage token can't sit in a manifest silently never
// matching. Widen this only if the worker side ever routes beyond FastAPI.
var allowedMethods = map[string]struct{}{
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	http.MethodPatch:   {},
	http.MethodDelete:  {},
	http.MethodOptions: {},
	http.MethodTrace:   {},
}

// Route is one validated manifest entry.
type Route struct {
	Template *Template
	Methods  []string // uppercase
	Public   bool
}

// Manifest is a worker's ordered route table.
type Manifest struct {
	routes []Route
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
		if ep.GetKind() != livekit.AgentHttp_AEK_HTTP {
			return nil, fmt.Errorf("endpoint %q has unsupported kind %s", ep.GetPath(), ep.GetKind())
		}
		if len(ep.GetMethods()) == 0 {
			return nil, fmt.Errorf("endpoint %q declares no methods", ep.GetPath())
		}
		var methods []string
		for _, method := range ep.GetMethods() {
			u := strings.ToUpper(method)
			if u != method {
				return nil, fmt.Errorf("endpoint %q method %q must be uppercase", ep.GetPath(), method)
			}
			if _, ok := allowedMethods[u]; !ok {
				return nil, fmt.Errorf("endpoint %q declares unsupported method %q", ep.GetPath(), method)
			}
			methods = append(methods, u)
		}
		m.routes = append(m.routes, Route{
			Template: tpl,
			Methods:  methods,
			Public:   ep.GetPublic(),
		})
	}
	return m, nil
}

// Match resolves a request path+method against the table: a FULL match on both,
// else PARTIAL if the path matched but no route had the method (405).
func (m *Manifest) Match(path, method string) (*Route, MatchResult) {
	partial := false
	for i := range m.routes {
		r := &m.routes[i]
		if !r.Template.Match(path) {
			continue
		}
		if slices.Contains(r.Methods, method) {
			return r, MatchFull
		}
		partial = true
	}
	if partial {
		return nil, MatchPartial
	}
	return nil, MatchNone
}

// slashAlternate returns the trailing-slash-normalized form of path when only
// that alternate form fully matches a registered route. The front tries the
// exact form first and uses this to rewrite a slash-mismatched request to the
// registered form and serve it directly, rather than redirecting the client
// (webhook clients often don't follow redirects, and a redirect from the final
// routing hop would pay the whole routing path twice). A route registered with
// a trailing slash is matched exactly and left untouched.
func (m *Manifest) slashAlternate(path, method string) (string, bool) {
	if path == "/" {
		return "", false
	}
	var alt string
	if strings.HasSuffix(path, "/") {
		alt = strings.TrimSuffix(path, "/")
	} else {
		alt = path + "/"
	}
	if _, res := m.Match(alt, method); res == MatchFull {
		return alt, true
	}
	return "", false
}
