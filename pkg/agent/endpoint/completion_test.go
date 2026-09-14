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
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// scriptedSession drives the front with a scripted worker writing raw HTTP/1.1.
type scriptedSession struct {
	script func(r io.Reader, w *workerSide)
}

func (s *scriptedSession) OpenStreams() int { return 0 }
func (s *scriptedSession) MaxStreams() int  { return DefaultMaxStreams }
func (s *scriptedSession) Close(string)     {}

func (s *scriptedSession) OpenStream(context.Context) (Stream, error) {
	toWorker, fromFront := net.Pipe()
	toFront, fromWorker := net.Pipe()
	st := &pipeStream{w: fromFront, r: toFront}

	go func() {
		s.script(toWorker, &workerSide{WriteCloser: fromWorker, st: st})
	}()
	return st, nil
}

// pipeStream is a Stream over a pair of net.Pipes, carrying a reset code the way
// a QUIC stream does.
type pipeStream struct {
	w io.WriteCloser
	r io.ReadCloser

	mu        sync.Mutex
	closed    bool
	peerReset *livekit.AgentHttp_HttpStreamResetCode
}

func (p *pipeStream) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err != nil {
		p.mu.Lock()
		code := p.peerReset
		p.mu.Unlock()
		if code != nil {
			return n, &StreamResetError{Code: *code}
		}
	}
	return n, err
}

func (p *pipeStream) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeStream) CloseWrite() error           { return p.w.Close() }
func (p *pipeStream) SetReadDeadline(t time.Time) error {
	type deadliner interface{ SetReadDeadline(time.Time) error }
	if d, ok := p.r.(deadliner); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}

func (p *pipeStream) Reset(livekit.AgentHttp_HttpStreamResetCode, string) { _ = p.Close() }

func (p *pipeStream) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	_ = p.w.Close()
	_ = p.r.Close()
	return nil
}

// workerSide is the scripted worker's end of the exchange.
type workerSide struct {
	io.WriteCloser
	st *pipeStream
}

// Reset ends the exchange the way a worker's RESET_STREAM does: the front's next
// read reports the code. The code is recorded before the close so the front
// cannot observe the EOF without it.
func (ws *workerSide) Reset(code livekit.AgentHttp_HttpStreamResetCode) {
	ws.st.mu.Lock()
	ws.st.peerReset = &code
	ws.st.mu.Unlock()
	_ = ws.WriteCloser.Close()
}

func (ws *workerSide) WriteString(s string) { _, _ = io.WriteString(ws.WriteCloser, s) }

// rawScriptedFront serves a front backed by one scripted worker whose side of
// the exchange is raw bytes.
func rawScriptedFront(t *testing.T, script func(r io.Reader, w *workerSide)) *httptest.Server {
	t.Helper()
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/stream", Methods: []string{"GET", "POST"}, Public: true},
	})
	require.NoError(t, err)

	reg := NewRegistry()
	r := &Registration{WorkerID: "w1", APIKey: "proj", AgentName: "a", Deployment: "d", Manifest: m}
	r.SetSession(&scriptedSession{script: script})
	require.NoError(t, reg.Register(r))

	ts := httptest.NewServer(NewFront(reg, grantedTo("proj"), logger.GetLogger()))
	t.Cleanup(ts.Close)
	return ts
}

// drainRequest reads the preamble and the whole request so the worker script can
// respond. net.Pipe is unbuffered, so a script that skips this deadlocks the
// front's writer.
func drainRequest(r io.Reader) *livekit.AgentHttp_StreamPreamble {
	pre, err := wire.ReadPreamble(r)
	if err != nil {
		return nil
	}
	req, err := http.ReadRequest(bufio.NewReader(r))
	if err != nil {
		return pre
	}
	_, _ = io.Copy(io.Discard, req.Body)
	return pre
}

func chunk(p []byte) string { return fmt.Sprintf("%x\r\n%s\r\n", len(p), p) }

const chunkedHead = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"

// A worker reports an incomplete body that the HTTP framing does not reveal: the
// chunked body terminates cleanly, so only the trailer carries the outcome.
func TestTrailersReportTruncationWithoutContentLength(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		defer w.Close()
		drainRequest(r)
		w.WriteString(chunkedHead)
		w.WriteString(chunk(make([]byte, 1000)))
		w.WriteString("0\r\nx-lk-completion: truncated\r\nx-lk-error: generator raised mid-stream\r\n\r\n")
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "a body the worker reported as truncated must not read as complete")
}

// A chunked body that never reaches its terminator is incomplete, whatever the
// stream does next.
func TestMissingTerminatorAbortsTheClient(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		defer w.Close()
		drainRequest(r)
		w.WriteString(chunkedHead)
		w.WriteString(chunk(make([]byte, 1000)))
		// no terminal 0-chunk
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()

	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "an unterminated body must not read as a clean EOF")
}

// A chunk header that overstates its payload must not reach the client as a
// complete body.
func TestTruncatedMidChunkAbortsTheClient(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		defer w.Close()
		drainRequest(r)
		w.WriteString(chunkedHead)
		w.WriteString("3e8\r\n")               // announces 1000 bytes
		w.WriteString(strings.Repeat("x", 10)) // delivers 10
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "a body cut short mid-chunk must not read as complete")
}

// A body that terminates with no completion trailer is exactly success.
func TestUnlengthedCompleteResponseSucceeds(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		defer w.Close()
		drainRequest(r)
		w.WriteString(chunkedHead)
		w.WriteString(chunk([]byte("streamed")))
		w.WriteString("0\r\n\r\n")
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "streamed", string(got))
}

// A refusal before any byte flowed is retryable and exhausts into a 503.
func TestRefusalBeforeHeadIsRetryable(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		mu.Lock()
		attempts++
		mu.Unlock()
		drainRequest(r)
		w.Reset(livekit.AgentHttp_HSR_REFUSED)
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, attempts, "one registration means one attempt, then the 503")
}

// A post-dispatch failure before the head is a 502 and is never retried.
func TestInternalBeforeHeadIsBadGateway(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		drainRequest(r)
		w.Reset(livekit.AgentHttp_HSR_INTERNAL)
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// A deadline overrun maps to 504. POST is not retryable, so the mapping reaches
// the client.
func TestTimeoutBeforeHeadIsGatewayTimeout(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		drainRequest(r)
		w.Reset(livekit.AgentHttp_HSR_TIMEOUT)
	})

	resp, err := http.Post(ts.URL+PathPrefix+"a/d/stream", "text/plain", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
}

// 101 must not reach the client as a switch the stream never performs.
func TestSwitchingProtocolsIsRejected(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		defer w.Close()
		drainRequest(r)
		w.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n")
	})

	resp, err := http.Post(ts.URL+PathPrefix+"a/d/stream", "text/plain", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// A retryable state with no other worker to try exhausts into a 503.
func TestRetryableStateExhaustsTo503(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		drainRequest(r)
		w.Reset(livekit.AgentHttp_HSR_TIMEOUT)
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "1", resp.Header.Get("Retry-After"))
}

// A reset with no code carries no claim that nothing was applied, so it is
// retried only for an idempotent method.
func TestAbortWithoutCodeIsIdempotentOnly(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		drainRequest(r)
		w.Reset(livekit.AgentHttp_HSR_ABORT)
	})

	resp, err := http.Post(ts.URL+PathPrefix+"a/d/stream", "text/plain", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"a POST must not be retried on an unexplained reset")
}

// The signalling namespace is the node's: a client that could set
// x-lk-completion could tell the worker a body it cut short was whole.
func TestClientSuppliedReservedHeadersAreStripped(t *testing.T) {
	seen := make(chan string, 1)
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		defer w.Close()
		_, _ = wire.ReadPreamble(r)
		req, err := http.ReadRequest(bufio.NewReader(r))
		if err != nil {
			seen <- "read error"
			return
		}
		seen <- req.Header.Get("X-Lk-Completion")
		w.WriteString("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
	})

	req, err := http.NewRequest(http.MethodGet, ts.URL+PathPrefix+"a/d/stream", nil)
	require.NoError(t, err)
	req.Header.Set("X-Lk-Completion", "internal")
	req.Header.Set("X-Lk-Error", "forged")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Empty(t, <-seen, "a client must not be able to forge a completion")
}

// Signalling between node and worker is not the client's to see.
func TestReservedResponseHeadersDoNotReachTheClient(t *testing.T) {
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		defer w.Close()
		drainRequest(r)
		w.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nx-lk-internal: leaked\r\n\r\nok")
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "ok", string(body))
	require.Empty(t, resp.Header.Get("X-Lk-Internal"))
}

// Header values are octets: a latin-1 filename must survive the hop.
func TestNonUTF8HeaderValueSurvives(t *testing.T) {
	const v = "attachment; filename=\"caf\xe9.txt\""
	ts := rawScriptedFront(t, func(r io.Reader, w *workerSide) {
		defer w.Close()
		drainRequest(r)
		w.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Disposition: " + v + "\r\n\r\n")
	})

	resp, err := http.Get(ts.URL + PathPrefix + "a/d/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, v, resp.Header.Get("Content-Disposition"))
}
