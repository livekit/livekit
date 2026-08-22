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
// HTTP endpoints data plane, used as test infrastructure: it registers a
// manifest over the control connection, attaches the fixed wire pool, and
// bridges each stream to a local HTTP server. The acceptance suite drives it to
// exercise the server end to end in Go, without a Python SDK worker.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
)

type Config struct {
	// ServerURL is the ws(s):// URL of the /agent endpoint
	ServerURL string
	APIKey    string
	APISecret string

	AgentName  string
	Deployment string
	Endpoints  []*livekit.AgentHttp_AgentEndpoint

	// TargetAddr is the host:port of the local HTTP server streams bridge into
	TargetAddr string

	Logger logger.Logger
}

// Worker is one registration epoch: a control connection plus the fixed
// data-connection pool.
type Worker struct {
	cfg        Config
	instanceID string

	mu         sync.Mutex
	workerID   string
	settings   *livekit.AgentHttp_AgentEndpointSettings
	control    *wsConn
	dataConns  []*dataConn
	closed     bool
	registered chan struct{}
	statusSeq  uint64
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

// Start registers and attaches the pool; it returns once the data plane is up.
func (w *Worker) Start(ctx context.Context) error {
	token, err := w.mintToken()
	if err != nil {
		return err
	}

	control, err := dialWS(ctx, w.cfg.ServerURL, token, false)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.control = control
	w.mu.Unlock()

	if err := control.writeWorker(&livekit.WorkerMessage{Message: &livekit.WorkerMessage_Register{
		Register: &livekit.RegisterWorkerRequest{
			Type:             livekit.JobType_JT_ROOM,
			AgentName:        w.cfg.AgentName,
			Version:          "endpoint-conformance-client",
			PingInterval:     30,
			Deployment:       w.cfg.Deployment,
			Endpoints:        w.cfg.Endpoints,
			InstanceId:       w.instanceID,
			EndpointProtocol: 1,
		},
	}}); err != nil {
		return err
	}

	go w.controlLoop(ctx, control)

	select {
	case <-w.registered:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("registration timeout")
	}

	w.mu.Lock()
	settings := w.settings
	workerID := w.workerID
	w.mu.Unlock()
	if settings == nil {
		return errors.New("server did not negotiate endpoint settings")
	}

	for i := uint32(0); i < settings.GetDataConnectionCount(); i++ {
		dc, err := w.attachDataConn(ctx, token, workerID, settings)
		if err != nil {
			return fmt.Errorf("attach %d: %w", i, err)
		}
		w.mu.Lock()
		w.dataConns = append(w.dataConns, dc)
		w.mu.Unlock()
	}

	// report availability so the front's load weighting sees the worker
	_ = w.UpdateStatus(livekit.WorkerStatus_WS_AVAILABLE, 0, false)

	return nil
}

func (w *Worker) mintToken() (string, error) {
	at := auth.NewAccessToken(w.cfg.APIKey, w.cfg.APISecret).
		SetVideoGrant(&auth.VideoGrant{Agent: true}).
		SetValidFor(24 * time.Hour)
	return at.ToJWT()
}

func (w *Worker) WorkerID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.workerID
}

func (w *Worker) UpdateStatus(status livekit.WorkerStatus, load float32, draining bool) error {
	w.mu.Lock()
	w.statusSeq++
	seq := w.statusSeq
	control := w.control
	w.mu.Unlock()
	if control == nil {
		return errors.New("not started")
	}
	return control.writeWorker(&livekit.WorkerMessage{Message: &livekit.WorkerMessage_UpdateWorker{
		UpdateWorker: &livekit.UpdateWorkerStatus{
			Status:   &status,
			Load:     load,
			Draining: draining,
			Seq:      seq,
		},
	}})
}

func (w *Worker) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	control := w.control
	conns := w.dataConns
	w.mu.Unlock()

	if control != nil {
		_ = control.close()
	}
	for _, dc := range conns {
		dc.close(errors.New("worker closed"))
	}
}

func (w *Worker) controlLoop(ctx context.Context, control *wsConn) {
	for {
		msg, err := control.readServer()
		if err != nil {
			return
		}
		switch m := msg.Message.(type) {
		case *livekit.ServerMessage_Register:
			w.mu.Lock()
			w.workerID = m.Register.GetWorkerId()
			w.settings = m.Register.GetEndpointSettings()
			w.mu.Unlock()
			close(w.registered)
		case *livekit.ServerMessage_Availability:
			// decline room jobs: the conformance client only serves endpoints
			_ = control.writeWorker(&livekit.WorkerMessage{Message: &livekit.WorkerMessage_Availability{
				Availability: &livekit.AvailabilityResponse{
					JobId:     m.Availability.GetJob().GetId(),
					Available: false,
				},
			}})
		case *livekit.ServerMessage_GoAway:
			w.cfg.Logger.Infow("server draining, closing", "reason", m.GoAway.GetReason())
			w.Close()
			return
		case *livekit.ServerMessage_Pong:
		default:
		}
	}
}

// --- data connections ---

type dataConn struct {
	ws     *wsConn
	params *livekit.AgentHttp_AttachDataConnectionResponse
	target string
	log    logger.Logger

	writeMu sync.Mutex // client side serializes writes; correctness over throughput

	mu      sync.Mutex
	streams map[uint32]*clientStream
	closed  bool

	// connection-level send window (response bytes toward the server), refilled
	// by server credit frames on stream 0
	sendMu         sync.Mutex
	sendCond       *sync.Cond
	connSendCredit int64

	// connection-level receive accounting (request bytes from the server)
	recvMu          sync.Mutex
	connRecvUnacked int64
}

func (w *Worker) attachDataConn(ctx context.Context, token, workerID string, settings *livekit.AgentHttp_AgentEndpointSettings) (*dataConn, error) {
	ws, err := dialWS(ctx, w.cfg.ServerURL, token, true)
	if err != nil {
		return nil, err
	}
	if err := ws.writeFrame(&livekit.AgentHttp_Frame{
		Message: &livekit.AgentHttp_Frame_Attach{Attach: &livekit.AgentHttp_AttachDataConnection{
			WorkerId:    workerID,
			InstanceId:  w.instanceID,
			AttachToken: settings.GetAttachToken(),
		}},
	}); err != nil {
		_ = ws.close()
		return nil, err
	}
	f, err := ws.readFrame()
	if err != nil {
		_ = ws.close()
		return nil, err
	}
	resp, ok := f.Message.(*livekit.AgentHttp_Frame_AttachResponse)
	if !ok {
		_ = ws.close()
		return nil, errors.New("expected attach response")
	}
	if e := resp.AttachResponse.GetError(); e != "" {
		_ = ws.close()
		return nil, fmt.Errorf("attach rejected: %s", e)
	}

	dc := &dataConn{
		ws:             ws,
		params:         resp.AttachResponse,
		target:         w.cfg.TargetAddr,
		log:            w.cfg.Logger,
		streams:        make(map[uint32]*clientStream),
		connSendCredit: int64(resp.AttachResponse.GetConnectionWindow()),
	}
	dc.sendCond = sync.NewCond(&dc.sendMu)
	ws.enableWireLiveness()
	go dc.readLoop()
	go dc.pingLoop()
	return dc, nil
}

// pingLoop keeps the wire's liveness visible in both directions: the server
// refreshes its read deadline on our pings, and our pong-refreshed deadline
// detects a dead server.
func (dc *dataConn) pingLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		dc.mu.Lock()
		closed := dc.closed
		dc.mu.Unlock()
		if closed {
			return
		}
		if err := dc.ws.ping(); err != nil {
			return
		}
	}
}

// writeFrame serializes all frames FIFO: unlike the server's scheduler there is
// no control/data priority here, so a credit can queue behind sibling data. Fine
// for a conformance client; bounded by the per-write deadline.
func (dc *dataConn) writeFrame(f *livekit.AgentHttp_Frame) error {
	dc.writeMu.Lock()
	defer dc.writeMu.Unlock()
	return dc.ws.writeFrame(f)
}

// reserveConnSend takes up to want bytes from the shared window.
func (dc *dataConn) reserveConnSend(want int64, failed func() bool) (int64, error) {
	dc.sendMu.Lock()
	defer dc.sendMu.Unlock()
	for dc.connSendCredit <= 0 {
		dc.mu.Lock()
		closed := dc.closed
		dc.mu.Unlock()
		if closed || failed() {
			return 0, errors.New("connection or stream closed")
		}
		dc.sendCond.Wait()
	}
	n := want
	if n > dc.connSendCredit {
		n = dc.connSendCredit
	}
	dc.connSendCredit -= n
	return n, nil
}

// connConsumed replenishes the shared receive window once request bytes were
// consumed, threshold-acked at half the window on stream 0.
func (dc *dataConn) connConsumed(n int64) {
	if n <= 0 {
		return
	}
	dc.recvMu.Lock()
	dc.connRecvUnacked += n
	var credit int64
	if dc.connRecvUnacked >= int64(dc.params.GetConnectionWindow())/2 {
		credit = dc.connRecvUnacked
		dc.connRecvUnacked = 0
	}
	dc.recvMu.Unlock()
	if credit > 0 {
		_ = dc.writeFrame(&livekit.AgentHttp_Frame{
			StreamId: 0,
			Message:  &livekit.AgentHttp_Frame_Credit{Credit: uint32(credit)},
		})
	}
}

func (dc *dataConn) close(err error) {
	dc.mu.Lock()
	if dc.closed {
		dc.mu.Unlock()
		return
	}
	dc.closed = true
	streams := make([]*clientStream, 0, len(dc.streams))
	for _, s := range dc.streams {
		streams = append(streams, s)
	}
	dc.streams = map[uint32]*clientStream{}
	dc.mu.Unlock()

	for _, s := range streams {
		s.fail(err)
	}
	dc.sendMu.Lock()
	dc.sendCond.Broadcast()
	dc.sendMu.Unlock()
	_ = dc.ws.close()
}

func (dc *dataConn) readLoop() {
	for {
		f, err := dc.ws.readFrame()
		if err != nil {
			dc.close(err)
			return
		}
		switch m := f.Message.(type) {
		case *livekit.AgentHttp_Frame_Open:
			dc.handleOpen(f.StreamId, m.Open)
		case *livekit.AgentHttp_Frame_Data:
			delivered := false
			dc.withStream(f.StreamId, func(s *clientStream) {
				s.onData(m.Data)
				delivered = true
			})
			if !delivered {
				// unknown stream: protocol contract says ignore, but the bytes
				// consumed the shared window and must be credited back
				dc.connConsumed(int64(len(m.Data)))
			}
		case *livekit.AgentHttp_Frame_Eof:
			dc.withStream(f.StreamId, func(s *clientStream) {
				s.onEOF()
			})
		case *livekit.AgentHttp_Frame_Reset_:
			dc.withStream(f.StreamId, func(s *clientStream) {
				s.fail(fmt.Errorf("reset: %s", m.Reset_.GetError()))
			})
		case *livekit.AgentHttp_Frame_Credit:
			if f.StreamId == 0 {
				dc.sendMu.Lock()
				dc.connSendCredit += int64(m.Credit)
				dc.sendCond.Broadcast()
				dc.sendMu.Unlock()
			} else {
				dc.withStream(f.StreamId, func(s *clientStream) {
					s.onCredit(m.Credit)
				})
			}
		default:
			// attach frames after the handshake are a protocol violation
			dc.close(errors.New("unexpected frame on data connection"))
			return
		}
	}
}

func (dc *dataConn) withStream(id uint32, f func(*clientStream)) {
	dc.mu.Lock()
	s := dc.streams[id]
	dc.mu.Unlock()
	if s != nil {
		f(s)
	}
}

func (dc *dataConn) handleOpen(id uint32, open *livekit.AgentHttp_HttpStreamOpen) {
	s := &clientStream{
		id:         id,
		dc:         dc,
		sendCredit: int64(dc.params.GetCreditWindow()),
		failCh:     make(chan struct{}),
	}
	s.sendCond = sync.NewCond(&s.sendMu)
	s.recvCond = sync.NewCond(&s.recvMu)
	_ = open

	dc.mu.Lock()
	if dc.closed || uint32(len(dc.streams)) >= dc.params.GetMaxStreamsPerConn() {
		dc.mu.Unlock()
		_ = dc.writeFrame(resetFrame(id, livekit.AgentHttp_HSR_REFUSED, "no stream capacity"))
		return
	}
	dc.streams[id] = s
	dc.mu.Unlock()

	go s.run()
}

func resetFrame(id uint32, code livekit.AgentHttp_HttpStreamResetCode, reason string) *livekit.AgentHttp_Frame {
	return &livekit.AgentHttp_Frame{
		StreamId: id,
		Message: &livekit.AgentHttp_Frame_Reset_{
			Reset_: &livekit.AgentHttp_HttpStreamReset{Code: code, Error: reason},
		},
	}
}

// clientStream bridges one stream to one TCP connection to the local HTTP
// server. The transport parses nothing.
type clientStream struct {
	id uint32
	dc *dataConn

	sendMu     sync.Mutex
	sendCond   *sync.Cond
	sendCredit int64
	sendFailed bool

	// recv side: a cond-guarded buffer, never a blocking channel - the wire
	// read loop must not stall behind one slow local app (bytes in flight are
	// already bounded by the credit window)
	recvMu      sync.Mutex
	recvCond    *sync.Cond
	recvBuf     [][]byte
	recvEOF     bool
	recvDone    bool // removed or failed: arriving bytes are settled immediately
	recvUnacked int64

	failCh   chan struct{}
	failOnce sync.Once
	err      error
}

func (s *clientStream) fail(err error) {
	s.failOnce.Do(func() {
		s.recvMu.Lock()
		s.err = err
		s.recvDone = true
		// bytes the app will never read are settled against the shared window
		for _, p := range s.recvBuf {
			s.dc.connConsumed(int64(len(p)))
		}
		s.recvBuf = nil
		s.recvCond.Broadcast()
		s.recvMu.Unlock()
		s.sendMu.Lock()
		s.sendFailed = true
		s.sendCond.Broadcast()
		s.sendMu.Unlock()
		close(s.failCh)
	})
}

func (s *clientStream) onData(payload []byte) {
	s.recvMu.Lock()
	if s.recvDone {
		s.recvMu.Unlock()
		// never consumed by the app: release the shared window immediately
		s.dc.connConsumed(int64(len(payload)))
		return
	}
	s.recvBuf = append(s.recvBuf, payload)
	s.recvCond.Broadcast()
	s.recvMu.Unlock()
}

func (s *clientStream) onEOF() {
	s.recvMu.Lock()
	s.recvEOF = true
	s.recvCond.Broadcast()
	s.recvMu.Unlock()
}

func (s *clientStream) onCredit(inc uint32) {
	s.sendMu.Lock()
	s.sendCredit += int64(inc)
	s.sendCond.Broadcast()
	s.sendMu.Unlock()
}

func (s *clientStream) remove() {
	// mark done BEFORE unlinking so a delivery racing the removal settles the
	// shared window instead of vanishing into an orphaned buffer
	s.recvMu.Lock()
	s.recvDone = true
	for _, p := range s.recvBuf {
		s.dc.connConsumed(int64(len(p)))
	}
	s.recvBuf = nil
	s.recvCond.Broadcast()
	s.recvMu.Unlock()

	s.dc.mu.Lock()
	delete(s.dc.streams, s.id)
	s.dc.mu.Unlock()
}

// nextRecv blocks for the next request chunk; ok=false means EOF (done=false)
// or stream failure (done=true).
func (s *clientStream) nextRecv() (payload []byte, ok bool, failed bool) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	for len(s.recvBuf) == 0 && !s.recvEOF && !s.recvDone {
		s.recvCond.Wait()
	}
	if len(s.recvBuf) > 0 {
		payload = s.recvBuf[0]
		s.recvBuf = s.recvBuf[1:]
		return payload, true, false
	}
	if s.recvDone {
		return nil, false, true
	}
	return nil, false, false
}

// run executes the pump: dial the local app, copy stream->app and app->stream
// with two-level credit accounting on both directions. This is deliberately the
// report's five-line tunnel plus flow control.
func (s *clientStream) run() {
	defer s.remove()

	app, err := net.DialTimeout("tcp", s.dc.target, 10*time.Second)
	if err != nil {
		// nothing was dispatched: REFUSED tells the server a retry is safe
		_ = s.dc.writeFrame(resetFrame(s.id, livekit.AgentHttp_HSR_REFUSED, "local app unreachable"))
		return
	}
	defer app.Close()

	done := make(chan struct{}, 2)

	// stream -> app (request bytes)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			payload, ok, failed := s.nextRecv()
			if failed {
				return
			}
			if !ok {
				// EOF after all buffered chunks drained
				if tc, ok := app.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				return
			}
			// enforce, consume, and replenish both windows as bytes reach the app
			if _, err := app.Write(payload); err != nil {
				s.fail(err)
				return
			}
			s.dc.connConsumed(int64(len(payload)))
			s.recvUnacked += int64(len(payload))
			if s.recvUnacked >= int64(s.dc.params.GetCreditWindow())/2 {
				_ = s.dc.writeFrame(&livekit.AgentHttp_Frame{
					StreamId: s.id,
					Message:  &livekit.AgentHttp_Frame_Credit{Credit: uint32(s.recvUnacked)},
				})
				s.recvUnacked = 0
			}
		}
	}()

	// app -> stream (response bytes), under the stream window and the wire's
	// shared connection window
	go func() {
		defer func() { done <- struct{}{} }()
		failed := func() bool {
			select {
			case <-s.failCh:
				return true
			default:
				return false
			}
		}
		buf := make([]byte, int(s.dc.params.GetMaxFrameSize()))
		for {
			n, rerr := app.Read(buf)
			if n > 0 {
				remaining := buf[:n]
				for len(remaining) > 0 {
					s.sendMu.Lock()
					for s.sendCredit <= 0 && !s.sendFailed {
						s.sendCond.Wait()
					}
					if s.sendFailed {
						s.sendMu.Unlock()
						return
					}
					want := int64(len(remaining))
					if want > s.sendCredit {
						want = s.sendCredit
					}
					s.sendMu.Unlock()

					c, err := s.dc.reserveConnSend(want, failed)
					if err != nil {
						return
					}
					s.sendMu.Lock()
					s.sendCredit -= c
					s.sendMu.Unlock()

					chunk := make([]byte, c)
					copy(chunk, remaining[:c])
					if err := s.dc.writeFrame(&livekit.AgentHttp_Frame{
						StreamId: s.id,
						Message:  &livekit.AgentHttp_Frame_Data{Data: chunk},
					}); err != nil {
						s.fail(err)
						return
					}
					remaining = remaining[c:]
				}
			}
			if rerr != nil {
				if rerr != io.EOF {
					s.fail(rerr)
				}
				_ = s.dc.writeFrame(&livekit.AgentHttp_Frame{
					StreamId: s.id,
					Message:  &livekit.AgentHttp_Frame_Eof{Eof: &livekit.AgentHttp_HttpStreamEof{}},
				})
				return
			}
		}
	}()

	<-done
	<-done
}

// --- websocket transport ---

type wsConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func dialWS(ctx context.Context, serverURL, token string, attach bool) (*wsConn, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("access_token", token)
	if attach {
		q.Set("attach", "1")
	}
	u.RawQuery = q.Encode()

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	return &wsConn{ws: ws}, nil
}

func (c *wsConn) writeWorker(msg *livekit.WorkerMessage) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.ws.WriteMessage(websocket.BinaryMessage, payload)
}

func (c *wsConn) readServer() (*livekit.ServerMessage, error) {
	_, payload, err := c.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	msg := &livekit.ServerMessage{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func (c *wsConn) writeFrame(f *livekit.AgentHttp_Frame) error {
	payload, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.ws.WriteMessage(websocket.BinaryMessage, payload)
}

func (c *wsConn) readFrame() (*livekit.AgentHttp_Frame, error) {
	_ = c.ws.SetReadDeadline(time.Now().Add(wireIdleTimeout))
	_, payload, err := c.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	f := &livekit.AgentHttp_Frame{}
	if err := proto.Unmarshal(payload, f); err != nil {
		return nil, err
	}
	return f, nil
}

const wireIdleTimeout = 2 * time.Minute

// enableWireLiveness arms a read deadline refreshed by any inbound traffic and
// by pongs to our pings.
func (c *wsConn) enableWireLiveness() {
	_ = c.ws.SetReadDeadline(time.Now().Add(wireIdleTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(wireIdleTimeout))
	})
}

func (c *wsConn) ping() error {
	return c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
}

func (c *wsConn) close() error {
	return c.ws.Close()
}
