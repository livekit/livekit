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
	"errors"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

var (
	ErrNotVP8                          = errors.New("not VP8")
	ErrOutOfOrderVP8PictureIdCacheMiss = errors.New("out-of-order VP8 picture id not found in cache")
	ErrFilteredVP8TemporalLayer        = errors.New("filtered VP8 temporal layer")
)

type CodecMunger interface {
	GetState() interface{}
	SeedState(state interface{})

	SetLast(extPkt *buffer.ExtPacket)
	UpdateOffsets(extPkt *buffer.ExtPacket)

	UpdateAndGet(extPkt *buffer.ExtPacket, snOutOfOrder bool, snHasGap bool, maxTemporal int32) (int, []byte, error)

	UpdateAndGetPadding(newPicture bool) ([]byte, error)
}
