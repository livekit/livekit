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
)

type Signallerv2Params struct {
	Logger logger.Logger
}

type signallerv2 struct {
	unimplemented

	params Signallerv2Params

	signalCache    *SignalCache
	signalFragment *SignalFragment
}

func NewSignallerv2(params Signallerv2Params) ParticipantSignaller {
	return &signallerv2{
		params: params,
		signalCache: NewSignalCache(SignalCacheParams{
			Logger: params.Logger,
		}),
		signalFragment: NewSignalFragment(SignalFragmentParams{
			Logger: params.Logger,
		}),
	}
}

func (s *signallerv2) SetLastProcessedRemoteMessageId(lastProcessedRemoteMessageId uint32) {
	s.signalCache.SetLastProcessedRemoteMessageId(lastProcessedRemoteMessageId)
}

func (s *signallerv2) SendConnectResponse(connectResponse *livekit.ConnectResponse) error {
	if connectResponse != nil {
		s.signalCache.Add(&livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_ConnectResponse{
				ConnectResponse: connectResponse,
			},
		})
	}
	return nil
}
