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
	// when the wait for a ReconnectResponse started, zero once the connection is open
	handshakeArmedAt time.Time
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
	if sink != nil && reason == types.SignallingCloseReasonResume {
		s.handshakeArmedAt = time.Now()
	} else {
		s.handshakeArmedAt = time.Time{}
	}
	s.resSinkMu.Unlock()

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

func (s *signallerAsyncBase) HandshakePending() bool {
	s.resSinkMu.Lock()
	armedAt := s.handshakeArmedAt
	if armedAt.IsZero() {
		s.resSinkMu.Unlock()
		return false
	}
	if time.Since(armedAt) <= handshakeWindow {
		s.resSinkMu.Unlock()
		return true
	}
	s.handshakeArmedAt = time.Time{}
	s.resSinkMu.Unlock()

	s.params.Logger.Warnw(
		"resumed connection did not open with a ReconnectResponse", nil,
		"armedAt", armedAt,
	)
	s.notifyHandshakeOpened()
	return false
}

func (s *signallerAsyncBase) OpenHandshake() {
	s.resSinkMu.Lock()
	wasPending := !s.handshakeArmedAt.IsZero()
	s.handshakeArmedAt = time.Time{}
	s.resSinkMu.Unlock()

	if wasPending {
		s.notifyHandshakeOpened()
	}
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
