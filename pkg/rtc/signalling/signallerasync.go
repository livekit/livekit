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
	"fmt"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"

	"google.golang.org/protobuf/proto"
)

type SignallerAsyncParams struct {
	Logger      logger.Logger
	Participant types.LocalParticipant
}

type signallerAsync struct {
	signallerUnimplemented

	params SignallerAsyncParams

	resSinkMu sync.Mutex
	resSink   routing.MessageSink
}

func NewSignallerAsync(params SignallerAsyncParams) ParticipantSignaller {
	return &signallerAsync{
		params: params,
	}
}

func (s *signallerAsync) SetResponseSink(sink routing.MessageSink) {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()
	s.resSink = sink
}

func (s *signallerAsync) GetResponseSink() routing.MessageSink {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()
	return s.resSink
}

// closes signal connection to notify client to resume/reconnect
func (s *signallerAsync) CloseSignalConnection(reason types.SignallingCloseReason) {
	sink := s.GetResponseSink()
	if sink == nil {
		return
	}

	s.params.Logger.Debugw("closing signal connection", "reason", reason, "connID", sink.ConnectionID())
	sink.Close()
	s.SetResponseSink(nil)
}

func (s *signallerAsync) WriteMessage(msg proto.Message) error {
	if msg == nil {
		return nil
	}

	if s.params.Participant.IsDisconnected() {
		return nil
	}

	if !s.params.Participant.IsReady() {
		typed, ok := msg.(*livekit.SignalResponse)
		if !ok {
			s.params.Logger.Warnw(
				"unknown message type", nil,
				"messageType", fmt.Sprintf("%T", msg),
			)
		}
		if typed.GetJoin() == nil {
			return nil
		}
	}

	sink := s.GetResponseSink()
	if sink == nil {
		if typed, ok := msg.(*livekit.SignalResponse); ok {
			s.params.Logger.Debugw(
				"could not send message to participant",
				"messageType", fmt.Sprintf("%T", typed.Message),
			)
		}
		return nil
	}

	err := sink.WriteMessage(msg)
	if utils.ErrorIsOneOf(err, psrpc.Canceled, routing.ErrChannelClosed) {
		if typed, ok := msg.(*livekit.SignalResponse); ok {
			s.params.Logger.Debugw(
				"could not send message to participant",
				"error", err,
				"messageType", fmt.Sprintf("%T", typed.Message),
			)
		}
		return nil
	} else if err != nil {
		if typed, ok := msg.(*livekit.SignalResponse); ok {
			s.params.Logger.Warnw(
				"could not send message to participant", err,
				"messageType", fmt.Sprintf("%T", typed.Message),
			)
		}
		return err
	}
	return nil
}
