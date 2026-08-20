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

// Package endpoint implements the server side of the agent HTTP endpoints data
// plane: worker-dialed wires carrying multiplexed streams, each stream one
// opaque HTTP exchange, flow-controlled per stream and per wire with a
// prioritized per-wire write scheduler. Wires speak AgentHttp.Frame
// exclusively: one frame per websocket binary message.
package endpoint

import (
	"errors"
	"time"

	"github.com/livekit/protocol/livekit"
)

// CurrentProtocol is the data-plane protocol version negotiated in
// RegisterWorkerResponse.endpoint_settings.
const CurrentProtocol = 1

var (
	ErrConnClosed     = errors.New("data connection closed")
	ErrStreamClosed   = errors.New("stream closed")
	ErrStreamRefused  = errors.New("stream refused by worker")
	ErrTooManyStreams = errors.New("too many open streams on connection")
	ErrProtocol       = errors.New("data plane protocol violation")
)

// WireConn is the transport under one data wire: whole binary websocket
// messages in, one Frame each. The write deadline is what turns a peer that
// stopped reading into a dead connection instead of a stuck writer goroutine.
type WireConn interface {
	WriteFrame(f *livekit.AgentHttp_Frame) error
	ReadFrame() (*livekit.AgentHttp_Frame, error)
	SetWriteDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	Close() error
}

// Settings are the registration-level parameters negotiated on the control
// connection. Wire-level parameters ride each wire's attach response instead:
// wires may be adopted by any node, and the adopting node's parameters govern.
type Settings struct {
	Protocol      uint32
	AttachToken   string
	DataConnCount uint32
}

// WireParams are one wire's flow-control parameters, chosen by the node that
// adopted it and announced in AttachDataConnectionResponse.
type WireParams struct {
	CreditWindow      uint32
	ConnectionWindow  uint32
	MaxFrameSize      uint32
	MaxStreamsPerConn uint32
}

const (
	DefaultDataConnCount     = 8
	DefaultCreditWindow      = 1 << 20 // 1MiB per stream
	DefaultConnectionWindow  = 4 << 20 // shared by all streams on a wire
	DefaultMaxFrameSize      = 64 << 10
	DefaultMaxStreamsPerConn = 64 // total open; parked streams included

	// writeStallTimeout bounds a single wire write; a peer that stops reading
	// kills the connection instead of freezing its sibling streams.
	writeStallTimeout = 30 * time.Second

	// connBufferBudget caps the aggregate queued-but-unwritten bytes per
	// connection so many streams times a full window cannot pin unbounded memory.
	connBufferBudget = 4 << 20
)

// WithDefaults fills zero fields with the package defaults.
func (p WireParams) WithDefaults() WireParams {
	if p.CreditWindow == 0 {
		p.CreditWindow = DefaultCreditWindow
	}
	if p.ConnectionWindow == 0 {
		p.ConnectionWindow = DefaultConnectionWindow
	}
	if p.MaxFrameSize == 0 {
		p.MaxFrameSize = DefaultMaxFrameSize
	}
	if p.MaxStreamsPerConn == 0 {
		p.MaxStreamsPerConn = DefaultMaxStreamsPerConn
	}
	return p
}

func (s Settings) Proto() *livekit.AgentHttp_AgentEndpointSettings {
	return &livekit.AgentHttp_AgentEndpointSettings{
		Protocol:            s.Protocol,
		AttachToken:         s.AttachToken,
		DataConnectionCount: s.DataConnCount,
	}
}
