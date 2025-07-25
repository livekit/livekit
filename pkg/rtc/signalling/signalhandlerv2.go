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
	protosignalling "github.com/livekit/protocol/signalling"
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
	signalReassembler            *protosignalling.SignalReassembler
}

func NewSignalHandlerv2(params SignalHandlerv2Params) ParticipantSignalHandler {
	return &signalhandlerv2{
		params: params,
		signalReassembler: protosignalling.NewSignalReassembler(protosignalling.SignalReassemblerParams{
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
			sequencer := clientMessage.GetSequencer()
			if sequencer == nil || sequencer.MessageId == 0 {
				s.params.Logger.Warnw(
					"skipping message without sequencer", nil,
					"messageType", fmt.Sprintf("%T", clientMessage),
				)
				continue
			}

			lprmi := s.lastProcessedRemoteMessageId.Load()
			if sequencer.MessageId <= lprmi {
				s.params.Logger.Infow(
					"duplicate in message stream",
					"last", lprmi,
					"current", clientMessage.Sequencer.MessageId,
				)
				continue
			}

			// SIGNALLING-V2-TODO: ask for replay if there are gaps
			if lprmi != 0 && sequencer.MessageId != lprmi+1 {
				s.params.Logger.Infow(
					"gap in message stream",
					"last", lprmi,
					"current", clientMessage.Sequencer.MessageId,
				)
			}

			switch payload := clientMessage.GetMessage().(type) {
			case *livekit.Signalv2ClientMessage_PublisherSdp:
				s.params.Participant.HandleOffer(protosignalling.FromProtoSessionDescription(payload.PublisherSdp))

			case *livekit.Signalv2ClientMessage_SubscriberSdp:
				s.params.Participant.HandleAnswer(protosignalling.FromProtoSessionDescription(payload.SubscriberSdp))

			case *livekit.Signalv2ClientMessage_Trickle:
				candidateInit, err := protosignalling.FromProtoTrickle(payload.Trickle)
				if err != nil {
					s.params.Logger.Warnw("could not decode trickle", err)
					return err
				}
				s.params.Participant.AddICECandidate(candidateInit, payload.Trickle.Target)
			}

			s.lastProcessedRemoteMessageId.Store(sequencer.MessageId)
			s.params.Signalling.AckMessageId(sequencer.LastProcessedRemoteMessageId)
			s.params.Signalling.SetLastProcessedRemoteMessageId(sequencer.MessageId)
		}

	case *livekit.Signalv2WireMessage_Fragment:
		bytes := s.signalReassembler.Reassemble(msg.Fragment)
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

func (s *signalhandlerv2) HandleEncodedMessage(data []byte) error {
	wireMessage := &livekit.Signalv2WireMessage{}
	if err := proto.Unmarshal(data, wireMessage); err != nil {
		return err
	}

	return s.HandleMessage(wireMessage)
}

func (s *signalhandlerv2) PruneStaleReassemblies() {
	s.signalReassembler.Prune()
}
