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

package service

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/urfave/negroni/v3"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/agent/endpoint"
)

// IsAgentEndpointPath reports whether an escaped request path targets the agent
// endpoint front. The path must already be normalized; see WithPathNormalization.
func IsAgentEndpointPath(escapedPath string) bool {
	return strings.HasPrefix(escapedPath, endpoint.PathPrefix)
}

// AgentRecovery recovers panics, re-panicking http.ErrAbortHandler so it reaches
// net/http and aborts the response.
func AgentRecovery(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		if rec == http.ErrAbortHandler {
			panic(rec)
		}
		err, ok := rec.(error)
		if !ok {
			err = fmt.Errorf("%v", rec)
		}
		logger.Errorw("panic serving agent endpoint", err, "path", r.URL.Path)
		// a committed response already has its status on the wire
		if nrw, ok := w.(negroni.ResponseWriter); !ok || !nrw.Written() {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}()
	next(w, r)
}
