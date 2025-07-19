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

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

type signallerAsyncBaseParams struct {
	Logger logger.Logger
}

type signallerAsyncBase struct {
	signallerUnimplemented

	params signallerAsyncBaseParams

	resSinkMu sync.Mutex
	resSink   routing.MessageSink
}

func newSignallerAsyncBase(params signallerAsyncBaseParams) *signallerAsyncBase {
	return &signallerAsyncBase{
		params: params,
	}
}

func (s *signallerAsyncBase) SetResponseSink(sink routing.MessageSink) {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()
	s.resSink = sink
}

func (s *signallerAsyncBase) GetResponseSink() routing.MessageSink {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()
	return s.resSink
}

// closes signal connection to notify client to resume/reconnect
func (s *signallerAsyncBase) CloseSignalConnection(reason types.SignallingCloseReason) {
	sink := s.GetResponseSink()
	if sink == nil {
		return
	}

	s.params.Logger.Debugw("closing signal connection", "reason", reason, "connID", sink.ConnectionID())
	sink.Close()
	s.SetResponseSink(nil)
}
