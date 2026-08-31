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
	"io"
)

// CurrentProtocol is the data-plane protocol version this server implements,
// negotiated in RegisterWorkerResponse.
const CurrentProtocol uint32 = 1

// SessionCloseOK is the WebTransport application close code for a normal session
// teardown; the human-readable reason travels in the close message.
const SessionCloseOK = 0

// ResetCode is why a stream was aborted.
type ResetCode int

const (
	// ResetCancel: the node/client abandoned the exchange (client disconnect,
	// timeout, retry elsewhere).
	ResetCancel ResetCode = iota
	// ResetRefused: the worker aborted before dispatching the request to the
	// application, so the request was never applied and is safe to retry.
	ResetRefused
)

// Session is the node's handle to one worker's data plane: a single
// WebTransport (QUIC) session that also carries the worker's control stream.
// The node opens a fresh Stream per HTTP exchange; QUIC provides the
// multiplexing and per-stream flow control, so there is no capsule layer.
type Session interface {
	// OpenStream opens a bidirectional stream toward the worker for one HTTP
	// exchange. It blocks until a stream can be opened (QUIC stream-limit
	// backpressure) or ctx is done.
	OpenStream(ctx context.Context) (Stream, error)
	// OpenStreams reports the streams currently open on this session - the
	// least-outstanding-requests signal for worker selection.
	OpenStreams() int
	// MaxStreams is the soft cap on concurrent streams, used to derive spare
	// capacity for node-selection weighting.
	MaxStreams() int
	// Close tears the session down.
	Close(reason string)
}

// Stream is one HTTP exchange over a Session: opaque HTTP/1.1 request bytes are
// written toward the worker and opaque response bytes are read back. CloseWrite
// half-closes the request side (the worker then sees EOF); Reset aborts both
// directions.
type Stream interface {
	io.Reader
	io.Writer
	// CloseWrite half-closes the send side once the request is fully written
	// (QUIC stream FIN).
	CloseWrite() error
	// Reset aborts the stream in both directions (QUIC RESET_STREAM /
	// STOP_SENDING) with the given code.
	Reset(code ResetCode, reason string)
	// Close releases the stream after a completed exchange.
	Close() error
	// BytesRead reports response bytes consumed so far; the retry boundary is
	// "no response byte arrived".
	BytesRead() int64
	// Refused reports whether the worker aborted the stream with ResetRefused,
	// i.e. the request was never dispatched and is safe to retry elsewhere.
	Refused() bool
}
