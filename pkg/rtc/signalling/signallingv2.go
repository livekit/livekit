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

	signalCache    *SignalCache
	signalFragment *SignalFragment
}

func NewSignallingv2(params Signallingv2Params) ParticipantSignalling {
	return &signallingv2{
		params: params,
		signalCache: NewSignalCache(SignalCacheParams{
			Logger: params.Logger,
		}),
		signalFragment: NewSignalFragment(SignalFragmentParams{
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
	if connectResponse == nil {
		return nil
	}

	serverMessage := &livekit.Signalv2ServerMessage{
		Message: &livekit.Signalv2ServerMessage_ConnectResponse{
			ConnectResponse: connectResponse,
		},
	}
	s.signalCache.Add(serverMessage)
	return &livekit.Signalv2WireMessage{
		Message: &livekit.Signalv2WireMessage_Envelope{
			Envelope: &livekit.Envelope{
				ServerMessages: []*livekit.Signalv2ServerMessage{serverMessage},
			},
		},
	}
}
