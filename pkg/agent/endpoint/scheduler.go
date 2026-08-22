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
	"slices"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

// scheduler serializes all writes on one wire through a single writer goroutine
// with two classes: control frames (OPEN/CREDIT/RESET) preempt DATA, and DATA
// drains one chunk per stream in round-robin order; a stream's EOF rides its
// data queue so it can never overtake the stream's own bytes. Without the class
// split, credits queue behind a deep data backlog and the peer's send windows
// never refill; without the round-robin, one heavy
// stream starves its siblings; without the budget, many streams times a full
// credit window pin unbounded memory; without the write deadline, a peer that
// stops reading freezes cancellations too.
//
// Locking rule: the wire write happens OUTSIDE the scheduler lock. Holding the
// lock across a blocking write would let one stalled connection stop enqueues
// from every stream sharing it.
// chunkPoolMinSize is the payload size at or above which a frame's buffer is
// drawn from the recycle pool. Below it (webhook-sized bodies, headers) a
// right-sized make is cheaper than borrowing a full MaxFrameSize buffer, so
// small/average requests never pay the pooled buffer's footprint.
const chunkPoolMinSize = 16 << 10

type scheduler struct {
	wire WireConn

	mu     sync.Mutex
	cond   *sync.Cond
	closed bool
	err    error

	control []*livekit.AgentHttp_Frame

	queues map[uint32]*streamQueue
	rr     []uint32 // stream ids with queued data, round-robin order
	rrIdx  int

	queuedBytes int64 // aggregate queued DATA payload bytes, capped by connBufferBudget

	// chunkPool recycles MaxFrameSize payload buffers for large frames (>=
	// chunkPoolMinSize), so a big upload does not allocate one per frame. A
	// buffer is drawn in Stream.Write, held in a streamQueue until its frame is
	// written, then returned (in writeLoop after WriteFrame, or in dropStream).
	chunkPool sync.Pool
}

type streamQueue struct {
	chunks []queuedChunk
	eof    bool // send an EOF frame after the last chunk drains
}

// queuedChunk is one pending DATA payload. data is the bytes to send; bp is the
// pooled buffer backing them and is non-nil only for pooled (large) chunks,
// which are the ones recycled once their frame is written.
type queuedChunk struct {
	data []byte
	bp   *[]byte
}

func newScheduler(wire WireConn, maxFrameSize int) *scheduler {
	s := &scheduler{
		wire:   wire,
		queues: make(map[uint32]*streamQueue),
		chunkPool: sync.Pool{New: func() any {
			b := make([]byte, maxFrameSize)
			return &b
		}},
	}
	s.cond = sync.NewCond(&s.mu)
	go s.writeLoop()
	return s
}

// newChunk copies p into a queued chunk, drawing a pooled MaxFrameSize buffer
// for large payloads and a right-sized make for small ones (so webhook-sized
// requests never borrow a full pooled buffer). The bytes are copied because the
// scheduler is async: the caller's buffer is reused before the frame is written.
func (s *scheduler) newChunk(p []byte) queuedChunk {
	if len(p) >= chunkPoolMinSize {
		bp := s.chunkPool.Get().(*[]byte)
		n := copy(*bp, p)
		return queuedChunk{data: (*bp)[:n], bp: bp}
	}
	data := make([]byte, len(p))
	copy(data, p)
	return queuedChunk{data: data}
}

// recycle returns a chunk's pooled buffer if it has one.
func (s *scheduler) recycle(c queuedChunk) {
	if c.bp != nil {
		s.chunkPool.Put(c.bp)
	}
}

// enqueueControl never blocks: control frames are small and bounded by the
// number of live streams.
func (s *scheduler) enqueueControl(f *livekit.AgentHttp_Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.errLocked()
	}
	s.control = append(s.control, f)
	s.cond.Broadcast()
	return nil
}

// enqueueData blocks while the connection buffer budget is exhausted. Stream
// and connection credit are enforced by the caller (Stream.Write); the budget
// is the local memory cap on top of them.
func (s *scheduler) enqueueData(streamID uint32, chunk queuedChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed && s.queuedBytes+int64(len(chunk.data)) > connBufferBudget && s.queuedBytes > 0 {
		s.cond.Wait()
	}
	if s.closed {
		s.recycle(chunk) // never enqueued; return its buffer to the pool
		return s.errLocked()
	}
	q := s.queues[streamID]
	if q == nil {
		q = &streamQueue{}
		s.queues[streamID] = q
	}
	if len(q.chunks) == 0 && !q.eof {
		s.rr = append(s.rr, streamID)
	}
	q.chunks = append(q.chunks, chunk)
	s.queuedBytes += int64(len(chunk.data))
	s.cond.Broadcast()
	return nil
}

// enqueueEOF marks the stream's send side done; the EOF frame is emitted after
// its queued chunks, preserving stream order.
func (s *scheduler) enqueueEOF(streamID uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.errLocked()
	}
	q := s.queues[streamID]
	if q == nil {
		q = &streamQueue{}
		s.queues[streamID] = q
		s.rr = append(s.rr, streamID)
	} else if len(q.chunks) == 0 && !q.eof {
		s.rr = append(s.rr, streamID)
	}
	q.eof = true
	s.cond.Broadcast()
	return nil
}

// dropStream discards any queued data for a reset stream. A writer blocked on
// the budget can still enqueue one late chunk after the reset; receivers
// tolerate frames for unknown stream ids by protocol contract.
func (s *scheduler) dropStream(streamID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q, ok := s.queues[streamID]; ok {
		for _, c := range q.chunks {
			s.queuedBytes -= int64(len(c.data))
			s.recycle(c)
		}
		delete(s.queues, streamID)
		if i := slices.Index(s.rr, streamID); i != -1 {
			s.rr = slices.Delete(s.rr, i, i+1)
			if s.rrIdx > i {
				s.rrIdx--
			}
		}
		s.cond.Broadcast()
	}
}

func (s *scheduler) close(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.err = err
	s.cond.Broadcast()
	s.mu.Unlock()
	_ = s.wire.Close()
}

func (s *scheduler) errLocked() error {
	if s.err != nil {
		return s.err
	}
	return ErrConnClosed
}

// next pops the next frame to write: all pending control first, then one DATA
// chunk (or the EOF sentinel) from the round-robin ring. recycle is the pooled
// buffer backing a DATA frame, to be returned once the frame is written (nil
// for control/EOF/unpooled frames).
func (s *scheduler) next() (f *livekit.AgentHttp_Frame, recycle *[]byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closed {
			return nil, nil, false
		}
		if len(s.control) > 0 {
			f = s.control[0]
			s.control = s.control[1:]
			return f, nil, true
		}
		if len(s.rr) > 0 {
			if s.rrIdx >= len(s.rr) {
				s.rrIdx = 0
			}
			id := s.rr[s.rrIdx]
			q := s.queues[id]
			if len(q.chunks) > 0 {
				chunk := q.chunks[0]
				q.chunks = q.chunks[1:]
				s.queuedBytes -= int64(len(chunk.data))
				recycle = chunk.bp
				f = &livekit.AgentHttp_Frame{
					StreamId: id,
					Message:  &livekit.AgentHttp_Frame_Data{Data: chunk.data},
				}
			}
			if len(q.chunks) == 0 {
				if q.eof {
					if f == nil {
						f = &livekit.AgentHttp_Frame{
							StreamId: id,
							Message:  &livekit.AgentHttp_Frame_Eof{Eof: &livekit.AgentHttp_HttpStreamEof{}},
						}
					} else {
						// keep the stream in the ring so the EOF drains next round
						s.rrIdx++
						s.cond.Broadcast()
						return f, recycle, true
					}
				}
				delete(s.queues, id)
				s.rr = slices.Delete(s.rr, s.rrIdx, s.rrIdx+1)
			} else {
				s.rrIdx++
			}
			s.cond.Broadcast() // budget freed
			return f, recycle, true
		}
		s.cond.Wait()
	}
}

func (s *scheduler) writeLoop() {
	for {
		f, recycle, ok := s.next()
		if !ok {
			return
		}
		_ = s.wire.SetWriteDeadline(time.Now().Add(writeStallTimeout))
		err := s.wire.WriteFrame(f)
		// WriteFrame marshalled the payload into its own buffer and gorilla does
		// not retain it, so the pooled buffer is free to recycle here whether or
		// not the write succeeded.
		if recycle != nil {
			s.chunkPool.Put(recycle)
		}
		if err != nil {
			s.close(err)
			return
		}
	}
}
