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
	"math/rand"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"go.uber.org/atomic"
)

const (
	defaultMaxFragmentSize = 8192
)

type SignalSegmenterParams struct {
	Logger          logger.Logger
	MaxFragmentSize int
	FirstPacketId   uint32 // should be used for testing only
}

type SignalSegmenter struct {
	params SignalSegmenterParams

	packetId atomic.Uint32
}

func NewSignalSegmenter(params SignalSegmenterParams) *SignalSegmenter {
	s := &SignalSegmenter{
		params: params,
	}
	if s.params.MaxFragmentSize == 0 {
		s.params.MaxFragmentSize = defaultMaxFragmentSize
	}
	s.packetId.Store(params.FirstPacketId)
	if s.packetId.Load() == 0 {
		s.packetId.Store(uint32(rand.Intn(1<<8) + 1))
	}
	return s
}

func (s *SignalSegmenter) Segment(data []byte) []*livekit.Fragment {
	if len(data) <= s.params.MaxFragmentSize {
		return nil
	}

	var fragments []*livekit.Fragment
	numFragments := uint32((len(data) + s.params.MaxFragmentSize - 1) / s.params.MaxFragmentSize)
	fragmentNumber := uint32(1)
	consumed := 0
	packetId := s.packetId.Inc()
	for len(data[consumed:]) != 0 {
		fragmentSize := min(len(data[consumed:]), s.params.MaxFragmentSize)
		fragment := &livekit.Fragment{
			PacketId:       packetId,
			FragmentNumber: fragmentNumber,
			NumFragments:   numFragments,
			FragmentSize:   uint32(fragmentSize),
			TotalSize:      uint32(len(data)),
			Data:           data[consumed : consumed+fragmentSize],
		}
		fragments = append(fragments, fragment)
		fragmentNumber++
		consumed += fragmentSize
	}

	return fragments
}
