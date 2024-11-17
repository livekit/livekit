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

package pacer

import (
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/protocol/logger"
)

type PassThrough struct {
	*Base
}

func NewPassThrough(logger logger.Logger, bwe bwe.BWE) *PassThrough {
	return &PassThrough{
		Base: NewBase(logger, bwe),
	}
}

func (p *PassThrough) Stop() {
}

func (p *PassThrough) Enqueue(pkt *Packet) {
	p.Base.SendPacket(pkt)
}

// ------------------------------------------------
