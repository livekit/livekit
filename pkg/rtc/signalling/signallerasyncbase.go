// Copyright 2023 LiveKit, Inc.
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

package signalling

import (
	"sync"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

type signallerAsyncBaseParams struct {
	Logger logger.Logger
	// called when the connection opens, i. e. when messages held back for the
	// handshake may be sent
	OnHandshakeOpened func()
}

// how long a resumed connection holds messages back waiting for its ReconnectResponse.
// A path that resumes without sending one lets messages flow after this instead of
// holding them back for the rest of the session.
const handshakeWindow = 5 * time.Second

type signallerAsyncBase struct {
	signallerUnimplemented

	params signallerAsyncBaseParams

	resSinkMu sync.Mutex
	resSink   routing.MessageSink
	// set while a resumed connection holds messages back waiting for its
	// ReconnectResponse, the timer opens the connection if none is written
	handshakePending bool
	handshakeTimer   *time.Timer
}

func newSignallerAsyncBase(params signallerAsyncBaseParams) *signallerAsyncBase {
	return &signallerAsyncBase{
		params: params,
	}
}

func (s *signallerAsyncBase) SwapResponseSink(sink routing.MessageSink, reason types.SignallingCloseReason) {
	s.resSinkMu.Lock()
	oldSink := s.resSink
	s.resSink = sink
	// a resumed connection has to open with the ReconnectResponse, the client takes it
	// only as the first message it reads
	opened := false
	switch {
	case sink == nil:
		// the connection is gone, keep anything queued for the next one
		s.disarmHandshakeLocked()
	case reason == types.SignallingCloseReasonResume:
		s.armHandshakeLocked()
	default:
		opened = s.disarmHandshakeLocked()
	}
	s.resSinkMu.Unlock()

	if opened {
		s.notifyHandshakeOpened()
	}

	if oldSink != nil {
		if sink != nil {
			s.params.Logger.Debugw(
				"swapping signal connection",
				"reason", reason,
				"connID", oldSink.ConnectionID(),
				"newConnID", sink.ConnectionID(),
			)
		} else {
			s.params.Logger.Debugw(
				"closing signal connection",
				"reason", reason,
				"connID", oldSink.ConnectionID(),
			)
		}
		oldSink.Close()
	}
}

// HandshakePending is a plain read, so a caller can decide to hold a message back
// while holding its own lock
func (s *signallerAsyncBase) HandshakePending() bool {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()

	return s.handshakePending
}

func (s *signallerAsyncBase) OpenHandshake() {
	s.resSinkMu.Lock()
	opened := s.disarmHandshakeLocked()
	s.resSinkMu.Unlock()

	if opened {
		s.notifyHandshakeOpened()
	}
}

func (s *signallerAsyncBase) armHandshakeLocked() {
	s.handshakePending = true
	if s.handshakeTimer != nil {
		s.handshakeTimer.Stop()
	}
	// the connection opens on its own if nothing writes a ReconnectResponse, a path
	// that resumes without one should not hold messages back for the whole session
	s.handshakeTimer = time.AfterFunc(handshakeWindow, s.onHandshakeTimeout)
}

// disarmHandshakeLocked reports whether it opened a connection that was holding
// messages back
func (s *signallerAsyncBase) disarmHandshakeLocked() bool {
	if s.handshakeTimer != nil {
		s.handshakeTimer.Stop()
		s.handshakeTimer = nil
	}

	wasPending := s.handshakePending
	s.handshakePending = false
	return wasPending
}

func (s *signallerAsyncBase) onHandshakeTimeout() {
	s.resSinkMu.Lock()
	opened := s.disarmHandshakeLocked()
	s.resSinkMu.Unlock()

	if !opened {
		return
	}

	s.params.Logger.Warnw("resumed connection did not open with a ReconnectResponse", nil)
	s.notifyHandshakeOpened()
}

// notifyHandshakeOpened runs without resSinkMu held, the callback sends messages
func (s *signallerAsyncBase) notifyHandshakeOpened() {
	if s.params.OnHandshakeOpened != nil {
		s.params.OnHandshakeOpened()
	}
}

func (s *signallerAsyncBase) GetResponseSink() routing.MessageSink {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()
	return s.resSink
}

// closes signal connection to notify client to resume/reconnect
func (s *signallerAsyncBase) CloseSignalConnection(reason types.SignallingCloseReason) {
	s.SwapResponseSink(nil, reason)
}
