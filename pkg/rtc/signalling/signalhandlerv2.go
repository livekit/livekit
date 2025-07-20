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
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/rtc/types"

	"go.uber.org/atomic"
)

var _ ParticipantSignalHandler = (*signalhandlerv2)(nil)

type SignalHandlerv2Params struct {
	Logger      logger.Logger
	Participant types.LocalParticipant
	Signalling  ParticipantSignalling
}

type signalhandlerv2 struct {
	signalhandlerUnimplemented

	params SignalHandlerv2Params

	// SIGNALLING-V2-TODO: have to set this properly for `ConnectRequest` coming via sync HTTP path
	lastProcessedRemoteMessageId atomic.Uint32
	signalFragment               *SignalFragment
}

func NewSignalHandlerv2(params SignalHandlerv2Params) ParticipantSignalHandler {
	return &signalhandlerv2{
		params: params,
		signalFragment: NewSignalFragment(SignalFragmentParams{
			Logger: params.Logger,
		}),
	}
}

func (s *signalhandlerv2) HandleRequest(msg proto.Message) error {
	req, ok := msg.(*livekit.Signalv2WireMessage)
	if !ok {
		s.params.Logger.Warnw(
			"unknown message type", nil,
			"messageType", fmt.Sprintf("%T", msg),
		)
		return ErrInvalidMessageType
	}

	// SIGNALLING-V2-TODO: check if this makes sense for data channel based signalling
	s.params.Participant.UpdateLastSeenSignal()

	switch msg := req.GetMessage().(type) {
	case *livekit.Signalv2WireMessage_Envelope:
		for _, clientMessage := range msg.Envelope.ClientMessages {
			if clientMessage.Sequencer.MessageId != s.lastProcessedRemoteMessageId.Load()+1 {
				s.params.Logger.Infow(
					"gap in message stream",
					"last", s.lastProcessedRemoteMessageId.Load(),
					"current", clientMessage.Sequencer.MessageId,
				)
			}

			// SIGNALLING-V2-TODO: process messages

			s.params.Signalling.AckMessageId(clientMessage.Sequencer.LastProcessedRemoteMessageId)
			s.params.Signalling.SetLastProcessedRemoteMessageId(clientMessage.Sequencer.MessageId)
		}

	case *livekit.Signalv2WireMessage_Fragment:
		bytes := s.signalFragment.Reassemble(msg.Fragment)
		if len(bytes) != 0 {
			wireMessage := &livekit.Signalv2WireMessage{}
			err := proto.Unmarshal(bytes, wireMessage)
			if err != nil {
				s.params.Logger.Warnw("could not unmarshal re-assembled packet", err)
				return err
			}

			s.HandleRequest(wireMessage)
		}
	}

	return nil
}
