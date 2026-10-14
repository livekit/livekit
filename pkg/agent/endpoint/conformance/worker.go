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

package conformance

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
)

type Config struct {
	// ServerURL is the https:// URL of the /agent endpoint (WebTransport).
	ServerURL string
	APIKey    string
	APISecret string

	AgentName  string
	Deployment string
	Endpoints  []*livekit.AgentHttp_AgentEndpoint

	// TargetAddr is the host:port of the local HTTP server streams bridge into.
	TargetAddr string

	// Insecure skips TLS verification (self-signed certs in tests).
	Insecure bool

	Logger logger.Logger
}

// Worker is one registration epoch: a single WebTransport session carrying the
// control stream and every HTTP-exchange stream.
type Worker struct {
	cfg        Config
	instanceID string

	mu         sync.Mutex
	sess       *webtransport.Session
	workerID   string
	protocol   uint32
	closed     bool
	registered chan struct{}
}

func New(cfg Config) *Worker {
	if cfg.Logger == nil {
		cfg.Logger = logger.GetLogger()
	}
	return &Worker{
		cfg:        cfg,
		instanceID: guid.New("AEI_"),
		registered: make(chan struct{}),
	}
}

// Start dials the WebTransport session, registers on the control stream, and
// begins serving node-opened streams. It returns once registration completes.
func (w *Worker) Start(ctx context.Context) error {
	token, err := w.mintToken()
	if err != nil {
		return err
	}

	tr := &webtransport.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: w.cfg.Insecure, //nolint:gosec // test/self-signed only
			NextProtos:         []string{http3.NextProtoH3},
		},
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	_, sess, err := tr.Dial(ctx, w.cfg.ServerURL, hdr)
	if err != nil {
		return fmt.Errorf("dial webtransport: %w", err)
	}
	w.mu.Lock()
	w.sess = sess
	w.mu.Unlock()

	// control stream: the worker opens it first, then registers
	control, err := sess.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	if err := wire.WriteControlMessage(control, &livekit.WorkerMessage{
		Message: &livekit.WorkerMessage_Register{
			Register: &livekit.RegisterWorkerRequest{
				Type:             livekit.JobType_JT_ROOM,
				AgentName:        w.cfg.AgentName,
				Version:          "endpoint-conformance-client",
				PingInterval:     30,
				Deployment:       w.cfg.Deployment,
				Endpoints:        w.cfg.Endpoints,
				InstanceId:       w.instanceID,
				EndpointProtocol: wire.CurrentProtocol,
			},
		},
	}); err != nil {
		return fmt.Errorf("write register: %w", err)
	}

	var resp livekit.ServerMessage
	if err := wire.ReadControlMessage(control, &resp); err != nil {
		return fmt.Errorf("read register response: %w", err)
	}
	reg := resp.GetRegister()
	if reg == nil {
		return fmt.Errorf("expected register response, got %T", resp.GetMessage())
	}
	settings := reg.GetEndpointSettings()
	if settings == nil {
		return fmt.Errorf("server accepted the registration without endpoint settings")
	}
	if p := settings.GetProtocol(); p < wire.MinProtocol || p > wire.CurrentProtocol {
		return fmt.Errorf("server negotiated endpoint protocol %d, this worker speaks %d..%d",
			p, wire.MinProtocol, wire.CurrentProtocol)
	}

	w.mu.Lock()
	w.workerID = reg.GetWorkerId()
	w.protocol = settings.GetProtocol()
	close(w.registered)
	w.mu.Unlock()

	go w.controlLoop(control)
	// the serve loop lives as long as the session, not the dial context (which
	// the caller may cancel as soon as Start returns)
	go w.serveLoop(sess)
	return nil
}

// WaitRegistered blocks until registration completes or ctx is done.
func (w *Worker) WaitRegistered(ctx context.Context) error {
	select {
	case <-w.registered:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timed out waiting for registration")
	}
}

// WorkerID returns the id assigned by the server (valid after registration).
func (w *Worker) WorkerID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.workerID
}

// Close tears down the session.
func (w *Worker) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	sess := w.sess
	w.mu.Unlock()
	if sess != nil {
		_ = sess.CloseWithError(wire.SessionCloseOK, "worker closed")
	}
}

func (w *Worker) mintToken() (string, error) {
	at := auth.NewAccessToken(w.cfg.APIKey, w.cfg.APISecret).
		SetVideoGrant(&auth.VideoGrant{Agent: true}).
		SetValidFor(24 * time.Hour)
	return at.ToJWT()
}

// controlLoop drains further control messages (availability requests are
// declined; the conformance worker takes no jobs).
func (w *Worker) controlLoop(control *webtransport.Stream) {
	for {
		var msg livekit.ServerMessage
		if err := wire.ReadControlMessage(control, &msg); err != nil {
			return
		}
		if a := msg.GetAvailability(); a != nil {
			_ = wire.WriteControlMessage(control, &livekit.WorkerMessage{
				Message: &livekit.WorkerMessage_Availability{
					Availability: &livekit.AvailabilityResponse{
						JobId:     a.GetJob().GetId(),
						Available: false,
					},
				},
			})
		}
	}
}

// serveLoop accepts node-opened streams (one HTTP exchange each) and bridges
// them to the local target. It runs until the session ends.
func (w *Worker) serveLoop(sess *webtransport.Session) {
	ctx := sess.Context()
	for {
		stream, err := sess.AcceptStream(ctx)
		if err != nil {
			return
		}
		go w.serve(ctx, stream)
	}
}

// serve bridges one HTTP exchange to the local target. After the preamble the
// stream is opaque HTTP/1.1 in both directions.
func (w *Worker) serve(ctx context.Context, stream *webtransport.Stream) {
	defer stream.Close()

	pre, err := wire.ReadPreamble(stream)
	if err != nil {
		resetStream(stream, livekit.AgentHttp_HSR_PROTOCOL)
		return
	}
	if pre.GetKind() != livekit.AgentHttp_AEK_HTTP {
		w.cfg.Logger.Infow("agent endpoint stream kind not served",
			"kind", pre.GetKind().String(), "requestID", pre.GetRequestId())
		resetStream(stream, livekit.AgentHttp_HSR_PROTOCOL)
		return
	}

	if ms := pre.GetTimeoutMs(); ms > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
		defer cancel()
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", w.cfg.TargetAddr)
	if err != nil {
		// the application never observed the request, so refused is sound here.
		// The code carries no detail, so the reason is logged against the
		// request ID instead.
		w.cfg.Logger.Infow("agent endpoint target dial failed",
			"error", err, "target", w.cfg.TargetAddr, "requestID", pre.GetRequestId())
		resetStream(stream, livekit.AgentHttp_HSR_REFUSED)
		return
	}
	defer conn.Close()

	// the deadline and the session ending both have to reach a blocked copy
	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		stream.CancelRead(wire.StreamCode(livekit.AgentHttp_HSR_ABORT))
	})
	defer stop()

	pipe(stream, conn)
}

// pipe copies the exchange in both directions until the target has finished
// answering. Neither direction is parsed.
func pipe(stream *webtransport.Stream, conn net.Conn) {
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		_, _ = io.Copy(conn, stream)
		// the request ended; the target needs the EOF to answer a body it read
		// to completion
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	_, _ = io.Copy(stream, conn)
	// the target is done answering, so nothing more of the request is wanted
	stream.CancelRead(wire.StreamCode(livekit.AgentHttp_HSR_ABORT))
	<-reqDone
}

// resetStream reports an outcome that happened before any HTTP bytes flowed,
// the only point at which a reset can carry one without racing them.
func resetStream(stream *webtransport.Stream, c livekit.AgentHttp_HttpStreamResetCode) {
	stream.CancelWrite(wire.StreamCode(c))
	stream.CancelRead(wire.StreamCode(c))
}
