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
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/livekit"
)

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
func (s *wtSession) Close(reason string) { _ = s.sess.CloseWithError(wire.SessionCloseOK, reason) }

// wtStream is one HTTP exchange over a WebTransport bidi stream.
type wtStream struct {
	sess *wtSession
	qs   *webtransport.Stream

	mu         sync.Mutex
	done       bool
	sendClosed bool // send side FIN'd (CloseWrite) or reset
}

func (s *wtStream) Read(p []byte) (int, error) {
	n, err := s.qs.Read(p)
	return n, translateStreamError(err)
}

func (s *wtStream) SetReadDeadline(t time.Time) error { return s.qs.SetReadDeadline(t) }

func (s *wtStream) Write(p []byte) (int, error) {
	n, err := s.qs.Write(p)
	return n, translateStreamError(err)
}

// CloseWrite sends STREAM FIN on the send side.
func (s *wtStream) CloseWrite() error {
	s.mu.Lock()
	s.sendClosed = true
	s.mu.Unlock()
	return s.qs.Close()
}

func (s *wtStream) Reset(code livekit.AgentHttp_HttpStreamResetCode, _ string) {
	s.mu.Lock()
	s.sendClosed = true
	s.mu.Unlock()
	c := wire.StreamCode(code)
	s.qs.CancelWrite(c)
	s.qs.CancelRead(c)
	s.release()
}

func (s *wtStream) Close() error {
	// If the send side was never cleanly FIN'd (CloseWrite) or reset, a writer
	// goroutine may still be blocked in Write because the peer stopped reading:
	// cancel the send side to unblock it (else the goroutine and QUIC stream leak).
	// After a clean FIN we must NOT reset: the body's own terminator followed by
	// FIN is the completion signal, and a reset destroys it.
	s.mu.Lock()
	cancelWrite := !s.sendClosed
	s.sendClosed = true
	s.mu.Unlock()
	abort := wire.StreamCode(livekit.AgentHttp_HSR_ABORT)
	if cancelWrite {
		s.qs.CancelWrite(abort)
	}
	s.qs.CancelRead(abort)
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

// translateStreamError turns a peer reset into the protocol's own error. Only a
// remote reset carries meaning: cancelling this side says nothing about what the
// worker did with the request.
func translateStreamError(err error) error {
	var se *webtransport.StreamError
	if errors.As(err, &se) && se.Remote {
		return &StreamResetError{Code: livekit.AgentHttp_HttpStreamResetCode(se.ErrorCode)}
	}
	return err
}
