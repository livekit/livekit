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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/router"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
)

const MaxManifestRoutes = 256

// Manifest is a worker's route table, in declaration order.
type Manifest = router.Router[*Route]

// Route is one validated manifest entry.
type Route struct {
	Template *router.Template
	Public   bool
}

// methodMask tags a route with the methods it serves. The set is exactly the
// verbs FastAPI can route; anything else masks to 0 and matches nothing.
func methodMask(method string) router.Mask {
	switch method {
	case http.MethodGet:
		return 1 << 0
	case http.MethodHead:
		return 1 << 1
	case http.MethodPost:
		return 1 << 2
	case http.MethodPut:
		return 1 << 3
	case http.MethodPatch:
		return 1 << 4
	case http.MethodDelete:
		return 1 << 5
	case http.MethodOptions:
		return 1 << 6
	case http.MethodTrace:
		return 1 << 7
	}
	return 0
}

// NegotiateSettings validates a registration's endpoint manifest and negotiates
// the data-plane protocol version. The data plane is a WebTransport session (no
// attach token, no fixed connection pool), so the version is all there is to
// agree on.
func NegotiateSettings(req *livekit.RegisterWorkerRequest) (*livekit.AgentHttp_AgentEndpointSettings, error) {
	if _, err := ParseManifest(req.GetEndpoints()); err != nil {
		return nil, err
	}
	if req.GetInstanceId() == "" {
		return nil, errors.New("registrations with endpoints require an instance_id")
	}
	if req.GetEndpointProtocol() == 0 {
		return nil, errors.New("worker declared endpoints but no endpoint protocol; upgrade the agent SDK to one that speaks the endpoint data plane")
	}
	// the worker frames to the version returned here, so it must be the
	// negotiated one and not the server's own constant
	negotiated := min(req.GetEndpointProtocol(), wire.CurrentProtocol)
	if negotiated < wire.MinProtocol {
		return nil, fmt.Errorf("unsupported agent endpoint protocol %d (this server serves %d..%d)",
			req.GetEndpointProtocol(), wire.MinProtocol, wire.CurrentProtocol)
	}
	return &livekit.AgentHttp_AgentEndpointSettings{Protocol: negotiated}, nil
}

// ParseManifest validates a registration's endpoint list and compiles it into
// a router.
func ParseManifest(endpoints []*livekit.AgentHttp_AgentEndpoint) (*Manifest, error) {
	if len(endpoints) > MaxManifestRoutes {
		return nil, fmt.Errorf("manifest exceeds %d routes", MaxManifestRoutes)
	}
	b := router.NewBuilder[*Route]()
	for _, ep := range endpoints {
		tpl, err := router.ParseTemplate(ep.GetPath())
		if err != nil {
			return nil, err
		}
		if ep.GetKind() != livekit.AgentHttp_AEK_HTTP {
			return nil, fmt.Errorf("endpoint %q has unsupported kind %s", ep.GetPath(), ep.GetKind())
		}
		if len(ep.GetMethods()) == 0 {
			return nil, fmt.Errorf("endpoint %q declares no methods", ep.GetPath())
		}
		var mask router.Mask
		for _, method := range ep.GetMethods() {
			u := strings.ToUpper(method)
			if u != method {
				return nil, fmt.Errorf("endpoint %q method %q must be uppercase", ep.GetPath(), method)
			}
			m := methodMask(u)
			if m == 0 {
				return nil, fmt.Errorf("endpoint %q declares unsupported method %q", ep.GetPath(), method)
			}
			mask |= m
		}
		if err := b.Add(tpl, mask, &Route{Template: tpl, Public: ep.GetPublic()}); err != nil {
			return nil, err
		}
	}
	return b.Build(), nil
}
