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
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/livekit/protocol/livekit"
)

// Stream is the server side of one multiplexed stream: one opaque HTTP
// exchange. Write carries request bytes toward the worker under the
// peer-granted stream window AND the wire's shared connection window; Read
// consumes response bytes and replenishes both windows as they are consumed, so
// a slow HTTP client suspends the worker's send without touching sibling
// streams.
type Stream struct {
	id   uint32
	conn *DataConn

	mu   sync.Mutex
	cond *sync.Cond

	// send side (request bytes)
	sendCredit  int64
	writeClosed bool

	// recv side (response bytes)
	recvChunks   [][]byte
	recvOffset   int   // read offset into recvChunks[0]
	recvWindow   int64 // bytes the peer may still send; enforced, not advisory
	recvUnacked  int64 // consumed bytes not yet credited back
	recvEOF      bool
	refused      bool
	err          error
	closed       bool
	bytesRead    int64
	bytesWritten int64
}

func newStream(id uint32, conn *DataConn, window int64) *Stream {
	s := &Stream{
		id:         id,
		conn:       conn,
		sendCredit: window,
		recvWindow: window,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *Stream) ID() uint32 { return s.id }

// BytesRead reports response bytes consumed so far; the retry boundary is
// "no response byte arrived".
func (s *Stream) BytesRead() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytesRead
}

// Refused reports whether the worker reset the stream with HSR_REFUSED, i.e.
// the request was never dispatched and is safe to retry elsewhere.
func (s *Stream) Refused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// Write sends request bytes toward the worker, blocking on the stream window,
// the wire's shared connection window and the local buffer budget.
func (s *Stream) Write(p []byte) (int, error) {
	cancelled := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closed || s.err != nil || s.writeClosed
	}
	total := 0
	for len(p) > 0 {
		s.mu.Lock()
		for s.sendCredit <= 0 && s.err == nil && !s.closed && !s.writeClosed {
			s.cond.Wait()
		}
		if err := s.writeErrLocked(); err != nil {
			s.mu.Unlock()
			return total, err
		}
		want := int64(len(p))
		if want > s.sendCredit {
			want = s.sendCredit
		}
		if max := int64(s.conn.params.MaxFrameSize); want > max {
			want = max
		}
		s.mu.Unlock()

		// the shared window is reserved after the stream window and outside
		// s.mu (the cancelled callback re-locks it)
		n, err := s.conn.reserveConnSend(want, cancelled)
		if err != nil {
			return total, err
		}

		s.mu.Lock()
		if err := s.writeErrLocked(); err != nil {
			s.mu.Unlock()
			s.conn.returnConnSend(n)
			return total, err
		}
		var overshoot int64
		if n > s.sendCredit {
			// single-writer streams never hit this; stay safe regardless
			overshoot = n - s.sendCredit
			n = s.sendCredit
		}
		s.sendCredit -= n
		s.bytesWritten += n
		s.mu.Unlock()
		s.conn.returnConnSend(overshoot)
		if n == 0 {
			// a concurrent writer drained the stream window between the peek
			// and the reservation: never emit an empty frame
			continue
		}

		chunk := make([]byte, n)
		copy(chunk, p[:n])
		if err := s.conn.sched.enqueueData(s.id, chunk); err != nil {
			return total, err
		}
		s.conn.noteActivity(n)
		p = p[n:]
		total += int(n)
	}
	return total, nil
}

func (s *Stream) writeErrLocked() error {
	switch {
	case s.err != nil:
		return s.err
	case s.closed:
		return ErrStreamClosed
	case s.writeClosed:
		return fmt.Errorf("%w: write after CloseWrite", ErrStreamClosed)
	}
	return nil
}

// CloseWrite half-closes the send side; the worker sees EOF after the queued
// request bytes drain.
func (s *Stream) CloseWrite() error {
	s.mu.Lock()
	if s.writeClosed || s.closed {
		s.mu.Unlock()
		return nil
	}
	s.writeClosed = true
	s.mu.Unlock()
	return s.conn.sched.enqueueEOF(s.id)
}

// Read consumes response bytes. Stream and connection credit are granted back
// to the worker on consumption (threshold acking at half the respective
// window) - receipt alone never replenishes either window.
func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	for len(s.recvChunks) == 0 && !s.recvEOF && s.err == nil && !s.closed {
		s.cond.Wait()
	}
	if len(s.recvChunks) == 0 {
		err := s.err
		if err == nil && s.recvEOF {
			err = io.EOF
		} else if err == nil {
			err = ErrStreamClosed
		}
		s.mu.Unlock()
		return 0, err
	}

	chunk := s.recvChunks[0]
	n := copy(p, chunk[s.recvOffset:])
	s.recvOffset += n
	if s.recvOffset == len(chunk) {
		s.recvChunks = s.recvChunks[1:]
		s.recvOffset = 0
	}
	s.bytesRead += int64(n)
	s.recvUnacked += int64(n)

	var credit int64
	if s.recvUnacked >= int64(s.conn.params.CreditWindow)/2 {
		credit = s.recvUnacked
		s.recvUnacked = 0
		s.recvWindow += credit
	}
	s.mu.Unlock()

	if credit > 0 {
		_ = s.conn.sched.enqueueControl(&livekit.AgentHttp_Frame{
			StreamId: s.id,
			Message:  &livekit.AgentHttp_Frame_Credit{Credit: uint32(credit)},
		})
	}
	s.conn.connConsumed(int64(n))
	s.conn.noteActivity(int64(n))
	return n, nil
}

// drainUnreadLocked releases response bytes that arrived but will never be
// Read, so a torn-down stream cannot leak the wire's connection window.
// Callers hold s.mu.
func (s *Stream) drainUnreadLocked() int64 {
	var pending int64
	for i, c := range s.recvChunks {
		pending += int64(len(c))
		if i == 0 {
			pending -= int64(s.recvOffset)
		}
	}
	s.recvChunks = nil
	s.recvOffset = 0
	return pending
}

// Reset aborts the stream in both directions.
func (s *Stream) Reset(code livekit.AgentHttp_HttpStreamResetCode, reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.err == nil {
		s.err = ErrStreamClosed
	}
	pending := s.drainUnreadLocked()
	s.cond.Broadcast()
	s.mu.Unlock()

	s.conn.connConsumed(pending)
	s.conn.wakeSendWaiters()
	s.conn.sched.dropStream(s.id)
	_ = s.conn.sched.enqueueControl(&livekit.AgentHttp_Frame{
		StreamId: s.id,
		Message: &livekit.AgentHttp_Frame_Reset_{
			Reset_: &livekit.AgentHttp_HttpStreamReset{Code: code, Error: reason},
		},
	})
	s.conn.removeStream(s.id)
}

// Close releases the stream after a completed exchange.
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.err == nil {
		s.err = ErrStreamClosed
	}
	pending := s.drainUnreadLocked()
	s.cond.Broadcast()
	s.mu.Unlock()

	s.conn.connConsumed(pending)
	s.conn.wakeSendWaiters()
	s.conn.sched.dropStream(s.id)
	s.conn.removeStream(s.id)
	return nil
}

// --- frame delivery from the wire read loop ---

func (s *Stream) onData(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// stream was reset locally; the conn window is released by the caller
		return errStreamGone
	}
	if int64(len(payload)) > s.recvWindow {
		return fmt.Errorf("%w: peer exceeded credit window on stream %d", ErrProtocol, s.id)
	}
	s.recvWindow -= int64(len(payload))
	s.recvChunks = append(s.recvChunks, payload)
	s.cond.Broadcast()
	return nil
}

// errStreamGone tells the read loop the payload was not adopted and its
// connection-window share must be released immediately.
var errStreamGone = errors.New("stream gone")

func (s *Stream) onEOF() {
	s.mu.Lock()
	s.recvEOF = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *Stream) onReset(code livekit.AgentHttp_HttpStreamResetCode, reason string) {
	s.mu.Lock()
	if s.err == nil {
		if code == livekit.AgentHttp_HSR_REFUSED {
			s.refused = true
			s.err = ErrStreamRefused
		} else {
			s.err = fmt.Errorf("stream reset by worker: %s (%s)", code, reason)
		}
	}
	pending := s.drainUnreadLocked()
	s.cond.Broadcast()
	s.mu.Unlock()
	s.conn.connConsumed(pending)
}

func (s *Stream) onCredit(increment uint32) {
	s.mu.Lock()
	s.sendCredit += int64(increment)
	s.cond.Broadcast()
	s.mu.Unlock()
}
