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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"google.golang.org/protobuf/proto"
)

var _ ParticipantSignalling = (*signallingv2)(nil)

type Signallingv2Params struct {
	Logger logger.Logger
}

type signallingv2 struct {
	signallingUnimplemented

	params Signallingv2Params

	signalCache *SignalCache
}

func NewSignallingv2(params Signallingv2Params) ParticipantSignalling {
	return &signallingv2{
		params: params,
		signalCache: NewSignalCache(SignalCacheParams{
			Logger: params.Logger,
		}),
	}
}

func (s *signallingv2) AckMessageId(ackMessageId uint32) {
	s.signalCache.Clear(ackMessageId)
}

func (s *signallingv2) SetLastProcessedRemoteMessageId(lastProcessedRemoteMessageId uint32) {
	s.signalCache.SetLastProcessedRemoteMessageId(lastProcessedRemoteMessageId)
}

func (s *signallingv2) PendingMessages() proto.Message {
	serverMessages := s.signalCache.GetFromFront()
	if len(serverMessages) == 0 {
		return nil
	}

	return &livekit.Signalv2WireMessage{
		Message: &livekit.Signalv2WireMessage_Envelope{
			Envelope: &livekit.Envelope{
				ServerMessages: serverMessages,
			},
		},
	}
}

func (s *signallingv2) SignalConnectResponse(connectResponse *livekit.ConnectResponse) proto.Message {
	serverMessage := &livekit.Signalv2ServerMessage{
		Message: &livekit.Signalv2ServerMessage_ConnectResponse{
			ConnectResponse: connectResponse,
		},
	}
	return s.cacheAndReturnEnvelope(serverMessage)
}

func (s *signallingv2) SignalSdpOffer(offer *livekit.SessionDescription) proto.Message {
	serverMessage := &livekit.Signalv2ServerMessage{
		Message: &livekit.Signalv2ServerMessage_SubscriberSdp{
			SubscriberSdp: offer,
		},
	}
	return s.cacheAndReturnEnvelope(serverMessage)
}

func (s *signallingv2) SignalSdpAnswer(answer *livekit.SessionDescription) proto.Message {
	serverMessage := &livekit.Signalv2ServerMessage{
		Message: &livekit.Signalv2ServerMessage_PublisherSdp{
			PublisherSdp: answer,
		},
	}
	return s.cacheAndReturnEnvelope(serverMessage)
}

func (s *signallingv2) SignalRoomUpdate(room *livekit.Room) proto.Message {
	serverMessage := &livekit.Signalv2ServerMessage{
		Message: &livekit.Signalv2ServerMessage_RoomUpdate{
			RoomUpdate: &livekit.RoomUpdate{
				Room: room,
			},
		},
	}
	return s.cacheAndReturnEnvelope(serverMessage)
}

func (s *signallingv2) SignalParticipantUpdate(participants []*livekit.ParticipantInfo) proto.Message {
	if len(participants) == 0 {
		return nil
	}

	serverMessage := &livekit.Signalv2ServerMessage{
		Message: &livekit.Signalv2ServerMessage_ParticipantUpdate{
			ParticipantUpdate: &livekit.ParticipantUpdate{
				Participants: participants,
			},
		},
	}
	return s.cacheAndReturnEnvelope(serverMessage)
}

func (s *signallingv2) cacheAndReturnEnvelope(sm *livekit.Signalv2ServerMessage) proto.Message {
	sm = s.signalCache.Add(sm)
	if sm == nil {
		return nil
	}

	return &livekit.Signalv2WireMessage{
		Message: &livekit.Signalv2WireMessage_Envelope{
			Envelope: &livekit.Envelope{
				ServerMessages: []*livekit.Signalv2ServerMessage{sm},
			},
		},
	}
}
