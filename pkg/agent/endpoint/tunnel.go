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
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

const (
	// DefaultTunnelDrainTimeout is how long a WebSocket tunnel outlives its
	// worker starting to drain.
	DefaultTunnelDrainTimeout = 30 * time.Second

	// bounds how late a worker's drain is noticed; draining is polled
	workerDrainPollInterval = time.Second

	// a client that stopped reading must not hold a drain open
	closeFrameWriteTimeout = time.Second

	// bounds a client that keeps its side open after the worker has closed
	tunnelLinger = 5 * time.Second
)

// a server-to-client close frame carrying 1001 (going away)
var wsGoingAwayCloseFrame = []byte{0x88, 0x02, 0x03, 0xe9}

func (f *Front) tunnelLimit(reg *Registration) int {
	if f.params.MaxTunnelsPerWorker > 0 {
		return f.params.MaxTunnelsPerWorker
	}
	return max(reg.maxStreams()/2, 1)
}

// excludeTunnelsAtLimit marks full workers as attempted, which sends an upgrade
// with no admissible worker to the fallback and then 503. A draining front
// admits none.
func (f *Front) excludeTunnelsAtLimit(workers []routeWorker, attempted map[*Registration]bool) {
	draining := f.tunnels.IsDraining()
	for _, w := range workers {
		if draining || w.reg.Tunnels() >= f.tunnelLimit(w.reg) {
			attempted[w.reg] = true
		}
	}
}

// Drain refuses new WebSocket upgrades, lets open tunnels finish until ctx is
// done, then closes the rest with 1001 (going away). It returns once no tunnel
// remains and is safe to call repeatedly.
func (f *Front) Drain(ctx context.Context) {
	f.tunnels.Drain(ctx)
}

// OpenTunnels reports the WebSocket tunnels this front is splicing.
func (f *Front) OpenTunnels() int {
	return f.tunnels.Len()
}

// spliceWebSocket blocks until the tunnel is done: bridge's deferred stream and
// reader cleanup must not run under a live tunnel.
func (f *Front) spliceWebSocket(
	w http.ResponseWriter,
	a *attempt,
	reg *Registration,
	stream Stream,
	br *bufio.Reader,
	resp *http.Response,
	writeErrCh <-chan error,
) {
	// the request writer must be done before the client's bytes follow its head
	if err := <-writeErrCh; err != nil {
		f.params.Logger.Warnw("agent endpoint upgrade request write failed", err,
			"workerID", reg.WorkerID, "path", a.escPath, "requestID", a.requestID)
		stream.Reset(livekit.AgentHttp_HSR_ABORT, "request write failed")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	conn, crw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		f.params.Logger.Warnw("agent endpoint cannot hijack the client connection", err,
			"workerID", reg.WorkerID, "path", a.escPath, "requestID", a.requestID)
		stream.Reset(livekit.AgentHttp_HSR_ABORT, "hijack failed")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer conn.Close()
	// server deadlines must not bound the tunnel
	_ = conn.SetDeadline(time.Time{})

	t := &tunnel{conn: conn, stream: stream, done: make(chan struct{})}
	admitted := f.tunnels.Add(t)
	defer f.tunnels.Remove(t)
	defer close(t.done)

	if _, err := conn.Write(webSocketSwitchHead(resp.Header)); err != nil {
		t.abort("client write failed")
		return
	}
	if !admitted {
		_ = t.Close()
	} else {
		go t.watch(reg, f.tunnels.Draining(), f.params.TunnelDrainTimeout)
	}

	bufp := f.pools.getBuf()
	defer f.pools.putBuf(bufp)
	t.pipe(br, crw.Reader, *bufp)
}

func webSocketSwitchHead(workerHeader http.Header) []byte {
	h := http.Header{}
	copyResponseHeaders(h, workerHeader)
	h.Set("Connection", "Upgrade")
	h.Set("Upgrade", "websocket")
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_ = h.Write(&b)
	b.WriteString("\r\n")
	return b.Bytes()
}

// watch covers worker drain only; once the front drains, Front.Drain owns the
// deadline.
func (t *tunnel) watch(reg *Registration, frontDraining <-chan struct{}, timeout time.Duration) {
	tick := time.NewTicker(workerDrainPollInterval)
	defer tick.Stop()
	for !reg.IsDraining() {
		select {
		case <-t.done:
			return
		case <-frontDraining:
			return
		case <-tick.C:
		}
	}
	if timeout <= 0 {
		timeout = DefaultTunnelDrainTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
		_ = t.Close()
	}
}

type tunnel struct {
	conn   net.Conn
	stream Stream
	done   chan struct{}

	// mu serializes client-bound writes, so a close frame never lands mid-frame
	mu     sync.Mutex
	closed bool
	frames wsFrameTracker
}

// pipe returns once both directions are done. EOF on one side half-closes the
// other; an error on either side tears down both.
func (t *tunnel) pipe(fromWorker, fromClient io.Reader, buf []byte) {
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		if _, err := io.Copy(t.stream, fromClient); err != nil {
			t.abort("client read failed")
			return
		}
		_ = t.stream.CloseWrite()
	}()

	if err := t.toClient(fromWorker, buf); err != io.EOF {
		t.abort("worker read failed")
	} else if cw, ok := t.conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		_ = t.conn.SetReadDeadline(time.Now().Add(tunnelLinger))
	} else {
		_ = t.conn.Close()
	}
	<-upDone
}

func (t *tunnel) toClient(src io.Reader, buf []byte) error {
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			t.mu.Lock()
			if t.closed {
				t.mu.Unlock()
				return net.ErrClosed
			}
			nw, werr := t.conn.Write(buf[:n])
			t.frames.advance(buf[:nw])
			t.mu.Unlock()
			if werr != nil {
				return werr
			}
		}
		if rerr != nil {
			return rerr
		}
	}
}

// Close sends the 1001 close frame only at a frame boundary, since a frame
// injected mid-frame corrupts the stream.
func (t *tunnel) Close() error {
	_ = t.conn.SetWriteDeadline(time.Now().Add(closeFrameWriteTimeout))
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		if t.frames.atBoundary() {
			_, _ = t.conn.Write(wsGoingAwayCloseFrame)
		}
	}
	t.mu.Unlock()
	t.abort("tunnel drained")
	return nil
}

func (t *tunnel) abort(reason string) {
	t.stream.Reset(livekit.AgentHttp_HSR_ABORT, reason)
	_ = t.conn.Close()
}

// wsFrameTracker follows the worker's framing (RFC 6455 §5.2) only far enough
// to find frame boundaries.
type wsFrameTracker struct {
	hdr       [14]byte
	hlen      int
	remaining uint64
}

func (f *wsFrameTracker) atBoundary() bool {
	return f.hlen == 0 && f.remaining == 0
}

func (f *wsFrameTracker) advance(p []byte) {
	for len(p) > 0 {
		if f.remaining > 0 {
			n := min(uint64(len(p)), f.remaining)
			f.remaining -= n
			p = p[n:]
			continue
		}
		f.hdr[f.hlen] = p[0]
		f.hlen++
		p = p[1:]
		if f.hlen < wsFrameHeaderLen(f.hdr[:f.hlen]) {
			continue
		}
		f.remaining = wsFramePayloadLen(f.hdr[:f.hlen])
		f.hlen = 0
	}
}

// wsFrameHeaderLen can grow as more of the header arrives.
func wsFrameHeaderLen(h []byte) int {
	if len(h) < 2 {
		return 2
	}
	n := 2
	switch h[1] & 0x7f {
	case 126:
		n += 2
	case 127:
		n += 8
	}
	if h[1]&0x80 != 0 {
		n += 4
	}
	return n
}

func wsFramePayloadLen(h []byte) uint64 {
	switch l := h[1] & 0x7f; l {
	case 126:
		return uint64(binary.BigEndian.Uint16(h[2:4]))
	case 127:
		return binary.BigEndian.Uint64(h[2:10])
	default:
		return uint64(l)
	}
}
