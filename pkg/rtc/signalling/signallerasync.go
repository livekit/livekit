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

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"

	"google.golang.org/protobuf/proto"
)

var _ ParticipantSignaller = (*signallerAsync)(nil)

type SignallerAsyncParams struct {
	Logger      logger.Logger
	Participant types.LocalParticipant
}

type signallerAsync struct {
	params SignallerAsyncParams

	*signallerAsyncBase
}

func NewSignallerAsync(params SignallerAsyncParams) ParticipantSignaller {
	return &signallerAsync{
		params:             params,
		signallerAsyncBase: newSignallerAsyncBase(signallerAsyncBaseParams{Logger: params.Logger}),
	}
}

func (s *signallerAsync) WriteMessage(msg proto.Message) error {
	if msg == nil {
		return nil
	}

	if s.params.Participant.IsDisconnected() {
		return nil
	}

	if !s.params.Participant.IsReady() {
		if typed, ok := msg.(*livekit.SignalResponse); !ok {
			s.params.Logger.Warnw(
				"unknown message type", nil,
				"messageType", fmt.Sprintf("%T", msg),
			)
		} else {
			if typed.GetJoin() == nil {
				return nil
			}
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
	if err != nil {
		if utils.ErrorIsOneOf(err, psrpc.Canceled, routing.ErrChannelClosed) {
			if typed, ok := msg.(*livekit.SignalResponse); ok {
				s.params.Logger.Debugw(
					"could not send message to participant",
					"error", err,
					"messageType", fmt.Sprintf("%T", typed.Message),
				)
			}
			return nil
		} else {
			if typed, ok := msg.(*livekit.SignalResponse); ok {
				s.params.Logger.Warnw(
					"could not send message to participant", err,
					"messageType", fmt.Sprintf("%T", typed.Message),
				)
			}
			return err
		}
	} else {
		s.params.Logger.Debugw("sent signal response", "response", logger.Proto(msg))
	}
	return nil
}
