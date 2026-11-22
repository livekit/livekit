// Copyright 2024 LiveKit, Inc.
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
	"net/http"
	"strconv"

	"github.com/gorilla/websocket"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc"
)

type AgentSocketUpgrader struct {
	websocket.Upgrader
}

func (u AgentSocketUpgrader) Upgrade(
	w http.ResponseWriter,
	r *http.Request,
	responseHeader http.Header,
) (
	conn *websocket.Conn,
	registration agent.WorkerRegistration,
	ok bool,
) {
	if u.CheckOrigin == nil {
		// allow connections from any origin, since script may be hosted anywhere
		// security is enforced by access tokens
		u.CheckOrigin = func(r *http.Request) bool {
			return true
		}
	}

	if !websocket.IsWebSocketUpgrade(r) {
		w.WriteHeader(404)
		return
	}

	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil || !claims.Video.Agent {
		HandleError(w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	registration = agent.MakeWorkerRegistration()
	registration.ClientIP = GetClientIP(r)

	conn, err := u.Upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		HandleError(w, r, http.StatusInternalServerError, err)
		return
	}

	if pv, err := strconv.Atoi(r.FormValue("protocol")); err == nil {
		registration.Protocol = agent.WorkerProtocolVersion(pv)
	}

	return conn, registration, true
}

// AgentWSService serves a worker's WebSocket control connection on /agent:
// control and job dispatch only, with no data-plane session for HTTP endpoints.
type AgentWSService struct {
	*AgentHandler

	upgrader               AgentSocketUpgrader
	signalMessageSizeLimit int64
}

func NewAgentWSService(conf *config.Config, h *AgentHandler) *AgentWSService {
	return &AgentWSService{
		AgentHandler:           h,
		signalMessageSizeLimit: conf.Limit.AgentSignalMessageSizeLimit,
	}
}

func (s *AgentWSService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if conn, registration, ok := s.upgrader.Upgrade(w, r, nil); ok {
		// bound a single signalling frame before it is buffered; 0 disables
		if s.signalMessageSizeLimit > 0 {
			conn.SetReadLimit(s.signalMessageSizeLimit)
		}
		sigConn := NewWSSignalConnection(conn, s.signalMessageSizeLimit)
		defer sigConn.Close()
		s.HandleConnection(r.Context(), sigConn, registration)
	}
}
