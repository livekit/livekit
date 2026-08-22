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
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// DataConn is one adopted worker wire carrying multiplexed streams. Only the
// server opens streams; ids are odd and scoped to the wire (even ids are
// reserved for worker-opened streams). Flow control is two-level: per-stream
// windows plus a shared connection window (stream 0 credit), so one stalled
// consumer can neither starve its siblings nor pin unbounded memory.
type DataConn struct {
	wire   WireConn
	params WireParams
	logger logger.Logger

	sched *scheduler

	mu       sync.Mutex
	streams  map[uint32]*Stream
	nextID   uint32
	closed   bool
	closeErr error
	onClose  func(*DataConn)

	// connection-level send window (request bytes toward the worker), refilled
	// by worker credit frames on stream 0
	sendMu         sync.Mutex
	sendCond       *sync.Cond
	connSendCredit int64

	// connection-level receive accounting (response bytes from the worker):
	// recvConnAvail is what the worker may still put in flight, replenished on
	// consumption via connConsumed
	recvMu          sync.Mutex
	recvConnAvail   int64
	recvConnUnacked int64

	// activity is an exponentially decayed byte counter used by stream
	// placement: a connection busy moving a heavy transfer should not receive
	// new streams while a lighter sibling exists.
	activity      atomic.Int64
	lastDecay     atomic.Int64 // unix nanos
	decayHalfLife time.Duration
}

func NewDataConn(wire WireConn, params WireParams, onClose func(*DataConn), log logger.Logger) *DataConn {
	params = params.WithDefaults()
	c := &DataConn{
		wire:           wire,
		params:         params,
		logger:         log,
		streams:        make(map[uint32]*Stream),
		nextID:         1, // odd ids; even reserved for worker-opened streams
		onClose:        onClose,
		connSendCredit: int64(params.ConnectionWindow),
		recvConnAvail:  int64(params.ConnectionWindow),
		decayHalfLife:  500 * time.Millisecond,
	}
	c.sendCond = sync.NewCond(&c.sendMu)
	c.lastDecay.Store(time.Now().UnixNano())
	c.sched = newScheduler(wire, int(params.MaxFrameSize))
	go c.readLoop()
	return c
}

// OpenStream opens a stream and sends its OPEN frame. It fails fast when the
// per-wire stream cap is reached; the caller picks another wire or worker.
func (c *DataConn) OpenStream(meta *livekit.AgentHttp_HttpStreamOpen) (*Stream, error) {
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		if err == nil {
			err = ErrConnClosed
		}
		return nil, err
	}
	if uint32(len(c.streams)) >= c.params.MaxStreamsPerConn {
		c.mu.Unlock()
		return nil, ErrTooManyStreams
	}
	id := c.nextID
	c.nextID += 2
	s := newStream(id, c, int64(c.params.CreditWindow))
	c.streams[id] = s
	c.mu.Unlock()

	if err := c.sched.enqueueControl(&livekit.AgentHttp_Frame{
		StreamId: id,
		Message:  &livekit.AgentHttp_Frame_Open{Open: meta},
	}); err != nil {
		c.removeStream(id)
		return nil, err
	}
	return s, nil
}

// OpenStreams reports open streams (active and parked).
func (c *DataConn) OpenStreams() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.streams)
}

// SpareStreams reports how many more streams this conn can accept before its
// per-conn cap; never negative.
func (c *DataConn) SpareStreams() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := int(c.params.MaxStreamsPerConn) - len(c.streams); n > 0 {
		return n
	}
	return 0
}

// HasCapacity reports whether a new stream may be opened.
func (c *DataConn) HasCapacity() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && uint32(len(c.streams)) < c.params.MaxStreamsPerConn
}

// --- connection-level send window ---

// reserveConnSend blocks until part of the shared window is available and
// reserves up to want bytes. cancelled lets a caller whose stream died stop
// waiting for credit that may never come.
func (c *DataConn) reserveConnSend(want int64, cancelled func() bool) (int64, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	for c.connSendCredit <= 0 {
		if c.closedErr() != nil {
			return 0, c.closedErr()
		}
		if cancelled() {
			return 0, ErrStreamClosed
		}
		c.sendCond.Wait()
	}
	if err := c.closedErr(); err != nil {
		return 0, err
	}
	n := want
	if n > c.connSendCredit {
		n = c.connSendCredit
	}
	c.connSendCredit -= n
	return n, nil
}

func (c *DataConn) returnConnSend(n int64) {
	if n <= 0 {
		return
	}
	c.sendMu.Lock()
	c.connSendCredit += n
	c.sendCond.Broadcast()
	c.sendMu.Unlock()
}

func (c *DataConn) addConnSend(increment uint32) {
	c.sendMu.Lock()
	c.connSendCredit += int64(increment)
	c.sendCond.Broadcast()
	c.sendMu.Unlock()
}

// wakeSendWaiters unblocks reserveConnSend callers so a reset stream's writer
// can observe its cancellation.
func (c *DataConn) wakeSendWaiters() {
	c.sendMu.Lock()
	c.sendCond.Broadcast()
	c.sendMu.Unlock()
}

func (c *DataConn) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		return nil
	}
	if c.closeErr != nil {
		return c.closeErr
	}
	return ErrConnClosed
}

// --- connection-level receive window ---

// consumeConnRecv accounts an arriving payload against the shared window.
func (c *DataConn) consumeConnRecv(n int64) error {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	if n > c.recvConnAvail {
		return ErrProtocol
	}
	c.recvConnAvail -= n
	return nil
}

// connConsumed replenishes the shared window once bytes are consumed (read by
// the client, or drained by a torn-down stream), threshold-acked at half the
// window on stream 0.
func (c *DataConn) connConsumed(n int64) {
	if n <= 0 {
		return
	}
	c.recvMu.Lock()
	c.recvConnUnacked += n
	var credit int64
	if c.recvConnUnacked >= int64(c.params.ConnectionWindow)/2 {
		credit = c.recvConnUnacked
		c.recvConnUnacked = 0
		c.recvConnAvail += credit
	}
	c.recvMu.Unlock()
	if credit > 0 {
		_ = c.sched.enqueueControl(&livekit.AgentHttp_Frame{
			StreamId: 0,
			Message:  &livekit.AgentHttp_Frame_Credit{Credit: uint32(credit)},
		})
	}
}

// noteActivity feeds the placement score: every payload byte in either
// direction counts, decayed over time so idle (parked) streams stop weighing.
func (c *DataConn) noteActivity(n int64) {
	c.decay()
	c.activity.Add(n)
}

func (c *DataConn) decay() {
	now := time.Now().UnixNano()
	last := c.lastDecay.Load()
	elapsed := time.Duration(now - last)
	if elapsed < c.decayHalfLife {
		return
	}
	if !c.lastDecay.CompareAndSwap(last, now) {
		return
	}
	halvings := int(elapsed / c.decayHalfLife)
	v := c.activity.Load()
	for i := 0; i < halvings && v != 0; i++ {
		v /= 2
	}
	c.activity.Store(v)
}

// Score is the placement weight: recent payload activity plus queued backlog.
// Lower is lighter. Heavy returns true when the connection is actively moving a
// large transfer and should not be co-located onto.
func (c *DataConn) Score() (score int64, heavy bool) {
	c.decay()
	a := c.activity.Load()
	c.sched.mu.Lock()
	q := c.sched.queuedBytes
	c.sched.mu.Unlock()
	score = a + q + int64(c.OpenStreams())*1024
	heavy = a+q >= int64(c.params.CreditWindow)/2
	return score, heavy
}

func (c *DataConn) Close(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	streams := make([]*Stream, 0, len(c.streams))
	for _, s := range c.streams {
		streams = append(streams, s)
	}
	c.streams = map[uint32]*Stream{}
	onClose := c.onClose
	c.mu.Unlock()

	if err == nil {
		err = ErrConnClosed
	}
	for _, s := range streams {
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.closed = true
		s.cond.Broadcast()
		s.mu.Unlock()
	}
	c.wakeSendWaiters()
	c.sched.close(err)
	if onClose != nil {
		onClose(c)
	}
}

func (c *DataConn) removeStream(id uint32) {
	c.mu.Lock()
	delete(c.streams, id)
	c.mu.Unlock()
}

func (c *DataConn) stream(id uint32) *Stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

func (c *DataConn) readLoop() {
	for {
		f, err := c.wire.ReadFrame()
		if err != nil {
			c.Close(err)
			return
		}
		switch m := f.Message.(type) {
		case *livekit.AgentHttp_Frame_Data:
			if err := c.consumeConnRecv(int64(len(m.Data))); err != nil {
				c.logger.Warnw("data plane protocol violation", err)
				c.Close(err)
				return
			}
			s := c.stream(f.StreamId)
			if s == nil {
				// reset locally; the payload will never be read
				c.connConsumed(int64(len(m.Data)))
				continue
			}
			c.noteActivity(int64(len(m.Data)))
			if err := s.onData(m.Data); err != nil {
				if err == errStreamGone {
					c.connConsumed(int64(len(m.Data)))
					continue
				}
				c.logger.Warnw("data plane protocol violation", err)
				c.Close(err)
				return
			}
		case *livekit.AgentHttp_Frame_Eof:
			if s := c.stream(f.StreamId); s != nil {
				s.onEOF()
			}
		case *livekit.AgentHttp_Frame_Reset_:
			if s := c.stream(f.StreamId); s != nil {
				s.onReset(m.Reset_.GetCode(), m.Reset_.GetError())
				c.removeStream(f.StreamId)
				c.sched.dropStream(f.StreamId)
				c.wakeSendWaiters()
			}
		case *livekit.AgentHttp_Frame_Credit:
			if f.StreamId == 0 {
				c.addConnSend(m.Credit)
			} else if s := c.stream(f.StreamId); s != nil {
				s.onCredit(m.Credit)
			}
		default:
			// attach handshakes ended before adoption; open only flows toward
			// the worker
			c.logger.Warnw("unexpected frame on data connection", ErrProtocol, "frame", logger.Proto(f))
			c.Close(ErrProtocol)
			return
		}
	}
}
