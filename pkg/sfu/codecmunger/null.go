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

package codecmunger

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
)

type Null struct {
	seededState interface{}
}

func NewNull(_logger logger.Logger) *Null {
	return &Null{}
}

func (n *Null) GetState() interface{} {
	return nil
}

func (n *Null) SeedState(state interface{}) {
	n.seededState = state
}

func (n *Null) GetSeededState() interface{} {
	return n.seededState
}

func (n *Null) SetLast(_extPkt *buffer.ExtPacket) {
}

func (n *Null) UpdateOffsets(_extPkt *buffer.ExtPacket) {
}

func (n *Null) UpdateAndGet(_extPkt *buffer.ExtPacket, snOutOfOrder bool, snHasGap bool, maxTemporal int32) (int, []byte, error) {
	return 0, nil, nil
}

func (n *Null) UpdateAndGetPadding(newPicture bool) ([]byte, error) {
	return nil, nil
}
