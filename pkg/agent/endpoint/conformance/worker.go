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

// Package conformance is a reference worker-side implementation of the agent
// HTTP endpoints data plane over WebTransport, used as test infrastructure: it
// opens one WebTransport session to /agent, registers its manifest on the
// control stream, then serves each node-opened stream by bridging the opaque
// HTTP exchange to a local HTTP server. The acceptance suite drives it to
// exercise the server end to end in Go, without a Python SDK worker.
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

	"github.com/livekit/livekit-server/pkg/agent/endpoint"
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
	if err := endpoint.WriteControlMessage(control, &livekit.WorkerMessage{
		Message: &livekit.WorkerMessage_Register{
			Register: &livekit.RegisterWorkerRequest{
				Type:             livekit.JobType_JT_ROOM,
				AgentName:        w.cfg.AgentName,
				Version:          "endpoint-conformance-client",
				PingInterval:     30,
				Deployment:       w.cfg.Deployment,
				Endpoints:        w.cfg.Endpoints,
				InstanceId:       w.instanceID,
				EndpointProtocol: endpoint.CurrentProtocol,
			},
		},
	}); err != nil {
		return fmt.Errorf("write register: %w", err)
	}

	var resp livekit.ServerMessage
	if err := endpoint.ReadControlMessage(control, &resp); err != nil {
		return fmt.Errorf("read register response: %w", err)
	}
	reg := resp.GetRegister()
	if reg == nil {
		return fmt.Errorf("expected register response, got %T", resp.GetMessage())
	}
	w.mu.Lock()
	w.workerID = reg.GetWorkerId()
	close(w.registered)
	w.mu.Unlock()

	go w.controlLoop(control)
	// the serve loop lives as long as the session, not the dial context (which
	// the caller may cancel as soon as Start returns)
	go w.serveLoop(sess)
	return nil
}

// controlLoop drains further control messages (availability requests are
// declined; the conformance worker takes no jobs).
func (w *Worker) controlLoop(control *webtransport.Stream) {
	for {
		var msg livekit.ServerMessage
		if err := endpoint.ReadControlMessage(control, &msg); err != nil {
			return
		}
		if a := msg.GetAvailability(); a != nil {
			_ = endpoint.WriteControlMessage(control, &livekit.WorkerMessage{
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
		go w.serve(stream)
	}
}

// serve bridges one HTTP exchange: the opaque request bytes on the stream are
// piped to a fresh TCP connection to the target, and the target's response
// bytes are piped back. Half-closes are propagated in both directions so
// streaming responses (SSE) flush incrementally.
func (w *Worker) serve(stream *webtransport.Stream) {
	tcp, err := net.Dial("tcp", w.cfg.TargetAddr)
	if err != nil {
		// the request was never dispatched: refuse so the front may retry
		stream.CancelWrite(endpoint.StreamCodeRefused)
		stream.CancelRead(endpoint.StreamCodeRefused)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// request: stream -> target, then half-close the target's write side
	go func() {
		defer wg.Done()
		_, _ = io.Copy(tcp, stream)
		if c, ok := tcp.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
	}()
	// response: target -> stream, then FIN the stream's response side
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stream, tcp)
		_ = stream.Close()
	}()
	wg.Wait()
	_ = tcp.Close()
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
		_ = sess.CloseWithError(endpoint.SessionCloseOK, "worker closed")
	}
}

func (w *Worker) mintToken() (string, error) {
	at := auth.NewAccessToken(w.cfg.APIKey, w.cfg.APISecret).
		SetVideoGrant(&auth.VideoGrant{Agent: true}).
		SetValidFor(24 * time.Hour)
	return at.ToJWT()
}
