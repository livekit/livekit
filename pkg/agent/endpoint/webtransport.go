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
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/quic-go/webtransport-go"
)

// Stream reset codes on the wire, shared by the server and the Go conformance
// worker so the two never drift. QUIC carries the numeric code on
// RESET_STREAM/STOP_SENDING; a worker uses Refused to mark a request it never
// dispatched (the front then treats it as safe to retry elsewhere).
const (
	StreamCodeCancel  webtransport.StreamErrorCode = 0
	StreamCodeRefused webtransport.StreamErrorCode = 2
)

func resetToWTCode(c ResetCode) webtransport.StreamErrorCode {
	if c == ResetRefused {
		return StreamCodeRefused
	}
	return StreamCodeCancel
}

// wtSession adapts a WebTransport session to Session: the node opens one stream
// per HTTP exchange and QUIC multiplexes them.
type wtSession struct {
	sess       *webtransport.Session
	maxStreams int
	open       atomic.Int64
}

// NewWebTransportSession wraps a live WebTransport session as a data-plane
// Session. maxStreams is the soft concurrency cap used only for capacity
// weighting; QUIC's own stream limit is the hard bound.
func NewWebTransportSession(sess *webtransport.Session, maxStreams int) Session {
	return &wtSession{sess: sess, maxStreams: maxStreams}
}

func (s *wtSession) OpenStream(ctx context.Context) (Stream, error) {
	qs, err := s.sess.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	s.open.Add(1)
	return &wtStream{sess: s, qs: qs}, nil
}

func (s *wtSession) OpenStreams() int    { return int(s.open.Load()) }
func (s *wtSession) MaxStreams() int     { return s.maxStreams }
func (s *wtSession) Close(reason string) { _ = s.sess.CloseWithError(0, reason) }

// wtStream is one HTTP exchange over a WebTransport bidi stream.
type wtStream struct {
	sess *wtSession
	qs   *webtransport.Stream

	mu         sync.Mutex
	bytesRead  int64
	refused    bool
	done       bool
	sendClosed bool // request side FIN'd (CloseWrite) or reset
}

func (s *wtStream) Read(p []byte) (int, error) {
	n, err := s.qs.Read(p)
	if n > 0 {
		s.mu.Lock()
		s.bytesRead += int64(n)
		s.mu.Unlock()
	}
	if err != nil {
		var se *webtransport.StreamError
		if errors.As(err, &se) && se.ErrorCode == StreamCodeRefused {
			s.mu.Lock()
			s.refused = true
			s.mu.Unlock()
		}
	}
	return n, err
}

func (s *wtStream) Write(p []byte) (int, error) { return s.qs.Write(p) }

// CloseWrite sends STREAM FIN on the request side; the worker then reads EOF.
func (s *wtStream) CloseWrite() error {
	s.mu.Lock()
	s.sendClosed = true
	s.mu.Unlock()
	return s.qs.Close()
}

func (s *wtStream) Reset(code ResetCode, _ string) {
	c := resetToWTCode(code)
	s.mu.Lock()
	s.sendClosed = true
	s.mu.Unlock()
	s.qs.CancelWrite(c)
	s.qs.CancelRead(c)
	s.release()
}

func (s *wtStream) Close() error {
	// If the request side was never cleanly FIN'd (CloseWrite) or reset, a writer
	// goroutine may still be blocked in Write because the worker stopped reading:
	// cancel the send side to unblock it (else the goroutine and QUIC stream leak).
	// After a clean FIN we must NOT reset - that would turn a completed request
	// into an abort on the wire.
	s.mu.Lock()
	cancelWrite := !s.sendClosed
	s.sendClosed = true
	s.mu.Unlock()
	if cancelWrite {
		s.qs.CancelWrite(StreamCodeCancel)
	}
	s.qs.CancelRead(StreamCodeCancel)
	s.release()
	return nil
}

// release decrements the session's open-stream count exactly once.
func (s *wtStream) release() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	s.mu.Unlock()
	s.sess.open.Add(-1)
}

func (s *wtStream) BytesRead() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytesRead
}

func (s *wtStream) Refused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}
