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

package rtc

import (
	"math/rand"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	defaultMaxFragmentSize = 8192
)

type reassembly struct {
	startedAt   time.Time
	fragments   []*livekit.Fragment
	isCorrupted bool
}

type SignalFragmentParams struct {
	Logger          logger.Logger
	MaxFragmentSize int
	FirstPacketId   uint32 // should be used for testing only
}

type SignalFragment struct {
	params SignalFragmentParams

	lock         sync.Mutex
	packetId     uint32
	reassemblies map[uint32]*reassembly
}

func NewSignalFragment(params SignalFragmentParams) *SignalFragment {
	s := &SignalFragment{
		params:       params,
		packetId:     params.FirstPacketId,
		reassemblies: make(map[uint32]*reassembly),
	}
	if s.params.MaxFragmentSize == 0 {
		s.params.MaxFragmentSize = defaultMaxFragmentSize
	}
	if s.packetId == 0 {
		s.packetId = uint32(rand.Intn(1<<8) + 1)
	}
	return s
}

func (s *SignalFragment) Segment(data []byte) []*livekit.Fragment {
	s.lock.Lock()
	defer s.lock.Unlock()

	if len(data) <= s.params.MaxFragmentSize {
		return nil
	}

	var fragments []*livekit.Fragment
	numFragments := uint32((len(data) + s.params.MaxFragmentSize - 1) / s.params.MaxFragmentSize)
	fragmentNumber := uint32(1)
	consumed := 0
	for len(data[consumed:]) != 0 {
		fragmentSize := min(len(data[consumed:]), s.params.MaxFragmentSize)
		fragment := &livekit.Fragment{
			PacketId:       s.packetId,
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

func (s *SignalFragment) Reassemble(fragment *livekit.Fragment) []byte {
	s.lock.Lock()
	defer s.lock.Unlock()

	re, ok := s.reassemblies[fragment.PacketId]
	if !ok {
		re = &reassembly{
			startedAt: time.Now(),
			fragments: make([]*livekit.Fragment, fragment.NumFragments),
		}
		s.reassemblies[fragment.PacketId] = re
	}
	if int(fragment.FragmentNumber) <= len(re.fragments) {
		if int(fragment.FragmentSize) != len(fragment.Data) {
			re.isCorrupted = true // runt packet, data size of blob does not match fragment size
		} else {
			re.fragments[fragment.FragmentNumber-1] = fragment
		}
	} else {
		re.isCorrupted = true
	}

	if re.isCorrupted {
		return nil
	}

	// try to reassemble
	expectedTotalSize := uint32(0)
	totalSize := 0
	for _, fr := range re.fragments {
		if fr == nil {
			return nil // not received all fragments of packet yet
		}

		expectedTotalSize = fr.TotalSize // can read this from any fragment of packet
		totalSize += len(fr.Data)
	}
	if expectedTotalSize != 0 && uint32(totalSize) != expectedTotalSize {
		re.isCorrupted = true
		return nil
	}

	data := make([]byte, 0, expectedTotalSize)
	for _, fr := range re.fragments {
		data = append(data, fr.Data...)
	}
	return data
}

// SIGNALLING-V2-TODO: need a prune worker to handle stale re-assemblies
