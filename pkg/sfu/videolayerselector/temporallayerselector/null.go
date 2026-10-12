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
)

// Null is for a stream whose temporal layers cannot be filtered, simulcast without the
// dependency descriptor for example: every temporal layer is forwarded. It reports the
// target as the current layer, so a target below the highest layer does not stay pending
// and block further allocation. The forwarder pairs it with the null codec munger, which
// ignores the temporal layer it returns for the packet.
type Null struct{}

func NewNull() *Null {
	return &Null{}
}

func (n *Null) Select(_extPkt *buffer.ExtPacket, _current int32, target int32) (this int32, next int32) {
	return target, target
}
