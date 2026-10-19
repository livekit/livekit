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
	"fmt"
	"io"
	"time"

	"github.com/livekit/protocol/livekit"
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

// Stream is one HTTP exchange over a Session: a preamble, then opaque HTTP/1.1
// bytes in each direction.
type Stream interface {
	io.Reader
	io.Writer
	// CloseWrite half-closes the send side (QUIC stream FIN). A FIN arriving
	// before the body framing says the body is complete is truncation.
	CloseWrite() error
	// Reset aborts the stream in both directions (QUIC RESET_STREAM /
	// STOP_SENDING). The code reaches the peer and is the only outcome signal
	// available once no bytes can flow; the reason is local only.
	Reset(code livekit.AgentHttp_HttpStreamResetCode, reason string)
	// Close releases the stream after a completed exchange.
	Close() error
	// SetReadDeadline bounds the wait for more bytes.
	SetReadDeadline(t time.Time) error
}

// StreamResetError reports that the peer reset the stream, carrying the code it
// sent. Transports translate their own reset errors into this at the Stream
// boundary.
type StreamResetError struct {
	Code livekit.AgentHttp_HttpStreamResetCode
}

func (e *StreamResetError) Error() string {
	return fmt.Sprintf("endpoint: stream reset by peer (%s)", e.Code)
}
