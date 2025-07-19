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
	"github.com/livekit/protocol/logger"
)

type Signallerv2HybridParams struct {
	Logger logger.Logger
}

type signallerv2Hybrid struct {
	params Signallerv2HybridParams

	*signallerv2Async
}

func NewSignallerv2Hybrid(params Signallerv2HybridParams) ParticipantSignaller {
	return &signallerv2Hybrid{
		params:           params,
		signallerv2Async: NewSignallerv2Async(Signallerv2AsyncParams{Logger: params.Logger}).(*signallerv2Async),
	}
}
