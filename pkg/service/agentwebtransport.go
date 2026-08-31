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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/endpoint"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// StartWebTransport starts the worker WebTransport listener (control + data) on
// the configured UDP port, if endpoints are enabled and a port is set. It
// returns a stop function (always non-nil). QUIC has no plaintext mode, so a
// TLS certificate is required: from tls_cert_file/tls_key_file, or a generated
// self-signed cert in dev mode.
func (s *AgentService) StartWebTransport(dev bool) (func(), error) {
	noop := func() {}
	cfg := s.endpointsConfig
	if cfg.Disabled || cfg.WebTransportPort == 0 {
		return noop, nil
	}
	tlsConf, err := WebTransportTLS(cfg.TLSCertFile, cfg.TLSKeyFile, dev)
	if err != nil {
		return noop, err
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: int(cfg.WebTransportPort)})
	if err != nil {
		return noop, err
	}
	wt := NewAgentWebTransportServer(s, s.keyProvider, tlsConf)
	go func() {
		if err := wt.Serve(udp); err != nil {
			logger.Infow("agent webtransport listener stopped", "error", err)
		}
	}()
	logger.Infow("agent webtransport listener started", "port", cfg.WebTransportPort)
	return func() { _ = wt.Close(); _ = udp.Close() }, nil
}

// WebTransportTLS builds the listener's TLS config from cert files, or a
// generated self-signed cert in dev mode. Shared by the OSS and cloud servers.
func WebTransportTLS(certFile, keyFile string, dev bool) (*tls.Config, error) {
	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{http3.NextProtoH3}}, nil
	}
	if !dev {
		return nil, errors.New("agent endpoints WebTransport requires agents.endpoints.tls_cert_file/tls_key_file")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "livekit-agent-webtransport-dev"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	logger.Warnw("agent webtransport using a generated self-signed cert (dev mode); workers must skip verification", nil)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{http3.NextProtoH3},
	}, nil
}

// NewWebTransportServer wraps an HTTP/3 WebTransport server around a handler.
// register is called with the constructed server so the caller can mount routes
// that Upgrade on it (Upgrade needs the *webtransport.Server); it returns the
// HTTP/3 handler. tlsConf must be usable for HTTP/3 (the h3 ALPN is set here if
// absent). Shared by the OSS and cloud agent servers.
func NewWebTransportServer(tlsConf *tls.Config, register func(*webtransport.Server) http.Handler) *webtransport.Server {
	tlsConf = tlsConf.Clone()
	if len(tlsConf.NextProtos) == 0 {
		tlsConf.NextProtos = []string{http3.NextProtoH3}
	}
	wt := &webtransport.Server{H3: &http3.Server{TLSConfig: tlsConf}}
	wt.H3.Handler = register(wt)
	return wt
}

// NewAgentWebTransportServer builds the WebTransport server that terminates a
// worker's unified session on /agent: the control stream carries the same
// WorkerMessage/ServerMessage exchange as the WebSocket control connection, and
// each HTTP exchange rides a node-opened QUIC stream on the same session. The
// handler runs behind the same api-key auth middleware as the rest of the
// agent surface, so the agent grant is enforced identically.
func NewAgentWebTransportServer(svc *AgentService, keyProvider auth.KeyProvider, tlsConf *tls.Config) *webtransport.Server {
	return NewWebTransportServer(tlsConf, func(wt *webtransport.Server) http.Handler {
		authMW := NewAPIKeyAuthMiddleware(keyProvider)
		mux := http.NewServeMux()
		mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
			svc.ServeWebTransport(wt, w, r)
		})
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authMW.ServeHTTP(w, r, mux.ServeHTTP)
		})
	})
}

// ServeWebTransport verifies the agent grant, upgrades the request to a
// WebTransport session, and serves it. It assumes an outer auth middleware has
// already placed the grant in the request context (as the WebSocket path does).
func (s *AgentService) ServeWebTransport(wt *webtransport.Server, w http.ResponseWriter, r *http.Request) {
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil || !claims.Video.Agent {
		HandleError(w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}
	apiKey := GetAPIKey(r.Context())

	registration := agent.MakeWorkerRegistration()
	registration.ClientIP = GetClientIP(r)
	if pv, err := strconv.Atoi(r.FormValue("protocol")); err == nil {
		registration.Protocol = agent.WorkerProtocolVersion(pv)
	}

	sess, err := wt.Upgrade(w, r)
	if err != nil {
		s.logger.Warnw("agent webtransport upgrade failed", err)
		return
	}

	// a detached context carries the worker's identity for the whole session;
	// the control loop exits on the control stream's read error when the session
	// dies, so it needs no cancellation from the request.
	ctx := WithGrants(context.Background(), claims, apiKey)
	go s.serveWebTransportSession(ctx, sess, registration)
}

func (s *AgentService) serveWebTransportSession(ctx context.Context, sess *webtransport.Session, registration agent.WorkerRegistration) {
	// the worker opens the control stream first (see the conformance worker and
	// the SDK); every other stream the node opens is an HTTP exchange. Bound the
	// wait so a session that upgrades but never opens a control stream can't hold
	// a goroutine until the QUIC idle timeout.
	acceptCtx, cancel := context.WithTimeout(sess.Context(), agent.RegisterTimeout)
	defer cancel()
	control, err := sess.AcceptStream(acceptCtx)
	if err != nil {
		_ = sess.CloseWithError(endpoint.SessionCloseOK, "no control stream")
		return
	}
	sigConn := NewWTSignalConn(sess, control)
	defer sigConn.Close()
	maxStreams := int(s.endpointsConfig.MaxStreams)
	if maxStreams <= 0 {
		maxStreams = endpoint.DefaultMaxStreams
	}
	epSession := endpoint.NewWebTransportSession(sess, maxStreams)
	s.handleConnection(ctx, sigConn, registration, epSession)
}

// wtSignalConn adapts a WebTransport control stream to agent.SignalConn: the
// same length-delimited WorkerMessage/ServerMessage exchange the WebSocket
// control connection carries, framed on one QUIC bidirectional stream.
type wtSignalConn struct {
	sess    *webtransport.Session
	control *webtransport.Stream
	writeMu sync.Mutex
}

// NewWTSignalConn adapts a WebTransport session's control stream to
// agent.SignalConn. Shared by the OSS and cloud agent servers.
func NewWTSignalConn(sess *webtransport.Session, control *webtransport.Stream) agent.SignalConn {
	return &wtSignalConn{sess: sess, control: control}
}

func (c *wtSignalConn) WriteServerMessage(msg *livekit.ServerMessage) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return 0, endpoint.WriteControlMessage(c.control, msg)
}

func (c *wtSignalConn) ReadWorkerMessage() (*livekit.WorkerMessage, int, error) {
	var msg livekit.WorkerMessage
	if err := endpoint.ReadControlMessage(c.control, &msg); err != nil {
		return nil, 0, err
	}
	return &msg, 0, nil
}

func (c *wtSignalConn) SetReadDeadline(t time.Time) error {
	return c.control.SetReadDeadline(t)
}

func (c *wtSignalConn) Close() error {
	return c.sess.CloseWithError(endpoint.SessionCloseOK, "")
}

func (c *wtSignalConn) CloseWithReason(reason string) error {
	return c.sess.CloseWithError(endpoint.SessionCloseOK, reason)
}
