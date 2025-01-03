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

package temporallayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
)

type VP8 struct {
	logger logger.Logger
}

func NewVP8(logger logger.Logger) *VP8 {
	return &VP8{
		logger: logger,
	}
}

func (v *VP8) Select(extPkt *buffer.ExtPacket, current int32, target int32) (this int32, next int32) {
	this = current
	next = current
	if current == target {
		return
	}

	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return
	}

	tid := extPkt.Temporal
	if current < target {
		if tid > current && tid <= target && vp8.S {
			this = tid
			next = tid
		}
	} else {
		if extPkt.Packet.Marker {
			next = target
		}
	}
	return
}
