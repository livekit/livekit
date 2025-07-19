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

type Signallerv2AsyncParams struct {
	Logger      logger.Logger
	Participant types.LocalParticipant
}

type signallerv2Async struct {
	params Signallerv2AsyncParams

	*signallerAsyncBase

	signalFragment *SignalFragment
}

func NewSignallerv2Async(params Signallerv2AsyncParams) ParticipantSignaller {
	return &signallerv2Async{
		params:             params,
		signallerAsyncBase: newSignallerAsyncBase(signallerAsyncBaseParams{Logger: params.Logger}),
		signalFragment: NewSignalFragment(SignalFragmentParams{
			Logger: params.Logger,
		}),
	}
}

// SIGNALLING-V2-TODO: need to lock write so that fragments do not get interrupted
func (s *signallerv2Async) WriteMessage(msg proto.Message) error {
	if msg == nil {
		return nil
	}

	if s.params.Participant.IsDisconnected() {
		return nil
	}

	if !s.params.Participant.IsReady() {
		if typed, ok := msg.(*livekit.Signalv2WireMessage); !ok {
			s.params.Logger.Warnw(
				"unknown message type", nil,
				"messageType", fmt.Sprintf("%T", msg),
			)
		} else {
			if !hasConnectResponse(typed) {
				return nil
			}
		}
	}

	sink := s.GetResponseSink()
	if sink == nil {
		if typed, ok := msg.(*livekit.Signalv2WireMessage); ok {
			s.params.Logger.Debugw(
				"could not send message to participant",
				"messageType", fmt.Sprintf("%T", typed.Message),
			)
		}
		return nil
	}

	// SIGNALLING-V2-TODO: avoid double marshalling,
	// have to marshal once to get size of serialised packet and decide if it needs fragmentation,
	// should used the marshaled bytes if fragmentation is not needed
	var fragments []*livekit.Fragment
	marshaled, err := proto.Marshal(msg)
	if err != nil {
		if typed, ok := msg.(*livekit.Signalv2WireMessage); ok {
			s.params.Logger.Warnw(
				"could not send message to participant", err,
				"messageType", fmt.Sprintf("%T", typed.Message),
			)
		}
	} else {
		fragments = s.signalFragment.Segment(marshaled)
	}

	sendMsg := func(m proto.Message) error {
		if err := sink.WriteMessage(m); err != nil {
			// SIGNALLING-V2-TODO: check for data channel errors to treat as debug too
			if utils.ErrorIsOneOf(err, psrpc.Canceled, routing.ErrChannelClosed) {
				if typed, ok := m.(*livekit.Signalv2WireMessage); ok {
					s.params.Logger.Debugw(
						"could not send message to participant",
						"error", err,
						"messageType", fmt.Sprintf("%T", typed.Message),
					)
				}
				return nil
			} else {
				if typed, ok := m.(*livekit.Signalv2WireMessage); ok {
					s.params.Logger.Warnw(
						"could not send message to participant", err,
						"messageType", fmt.Sprintf("%T", typed.Message),
					)
				}
				return err
			}
		}

		return nil
	}

	if len(fragments) != 0 {
		for _, fragment := range fragments {
			wireMessage := &livekit.Signalv2WireMessage{
				Message: &livekit.Signalv2WireMessage_Fragment{
					Fragment: fragment,
				},
			}
			if err := sendMsg(wireMessage); err != nil {
				return err
			}
		}
	} else {
		if err := sendMsg(msg); err != nil {
			return err
		}
	}
	return nil
}

// ----------------------------

func hasConnectResponse(wireMessage *livekit.Signalv2WireMessage) bool {
	switch msg := wireMessage.GetMessage().(type) {
	case *livekit.Signalv2WireMessage_Envelope:
		for _, innerMsg := range msg.Envelope.GetServerMessages() {
			switch innerMsg.GetMessage().(type) {
			case *livekit.Signalv2ServerMessage_ConnectResponse:
				return true

			default:
				return false // first message should be `ConnectResponse`
			}
		}

	default:
		// SIGNALLING-V2-TODO: handle ConnectResponse getting fragmented.
		return false
	}

	return false
}
