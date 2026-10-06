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
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/endpoint"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/livekit-server/pkg/rtc"
)

// AgentWTService serves a worker's WebTransport session on /agent: one control
// stream, plus a node-opened QUIC stream per HTTP exchange.
type AgentWTService struct {
	*AgentHandler
}

func NewAgentWTService(h *AgentHandler) *AgentWTService {
	return &AgentWTService{AgentHandler: h}
}

func (s *AgentWTService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, registration, ok := UpgradeAgentWebTransport(w, r, s.logger)
	if !ok {
		return
	}

	sigConn, epSession, ok := AcceptAgentControlStream(sess, s.endpointsConfig.MaxStreams)
	if !ok {
		return
	}
	defer sigConn.Close()

	s.handleConnection(r.Context(), sigConn, registration, epSession)
}

// UpgradeAgentWebTransport verifies the agent grant, builds the worker's
// registration from the request and upgrades to a WebTransport session. An outer
// auth middleware must already have placed the grant in the request context.
func UpgradeAgentWebTransport(w http.ResponseWriter, r *http.Request, l logger.Logger) (
	sess *webtransport.Session,
	registration agent.WorkerRegistration,
	ok bool,
) {
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil || !claims.Video.Agent {
		HandleError(w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	registration = agent.MakeWorkerRegistration()
	registration.ClientIP = GetClientIP(r)
	if pv, err := strconv.Atoi(r.FormValue("protocol")); err == nil {
		registration.Protocol = agent.WorkerProtocolVersion(pv)
	}

	sess, err := UpgradeWebTransport(w, r)
	if err != nil {
		l.Warnw("agent webtransport upgrade failed", err)
		return nil, registration, false
	}

	return sess, registration, true
}

// AcceptAgentControlStream waits for the worker's control stream and wraps the
// session for the data plane. The wait is bounded so a session that upgrades but
// never opens a control stream can't hold a goroutine until the QUIC idle timeout.
func AcceptAgentControlStream(sess *webtransport.Session, maxStreams uint32) (agent.SignalConn, endpoint.Session, bool) {
	acceptCtx, cancel := context.WithTimeout(sess.Context(), agent.RegisterTimeout)
	defer cancel()
	control, err := sess.AcceptStream(acceptCtx)
	if err != nil {
		_ = sess.CloseWithError(wire.SessionCloseOK, "no control stream")
		return nil, nil, false
	}
	if maxStreams == 0 {
		maxStreams = endpoint.DefaultMaxStreams
	}
	return NewWTSignalConn(sess, control), endpoint.NewWebTransportSession(sess, int(maxStreams)), true
}

// wtSignalConn adapts a WebTransport control stream to agent.SignalConn: a
// length-delimited WorkerMessage/ServerMessage exchange on one QUIC bidirectional
// stream.
type wtSignalConn struct {
	sess    *webtransport.Session
	control *webtransport.Stream
	writeMu sync.Mutex
}

func NewWTSignalConn(sess *webtransport.Session, control *webtransport.Stream) agent.SignalConn {
	return &wtSignalConn{sess: sess, control: control}
}

func (c *wtSignalConn) WriteServerMessage(msg *livekit.ServerMessage) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return 0, wire.WriteControlMessage(c.control, msg)
}

func (c *wtSignalConn) ReadWorkerMessage() (*livekit.WorkerMessage, int, error) {
	var msg livekit.WorkerMessage
	if err := wire.ReadControlMessage(c.control, &msg); err != nil {
		return nil, 0, err
	}
	return &msg, 0, nil
}

func (c *wtSignalConn) SetReadDeadline(t time.Time) error {
	return c.control.SetReadDeadline(t)
}

func (c *wtSignalConn) Close() error {
	return c.sess.CloseWithError(wire.SessionCloseOK, "")
}

func (c *wtSignalConn) CloseWithReason(reason string) error {
	return c.sess.CloseWithError(wire.SessionCloseOK, reason)
}
