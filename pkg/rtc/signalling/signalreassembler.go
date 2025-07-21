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
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"go.uber.org/zap/zapcore"
)

const (
	reassemblerTimeout = time.Minute
)

type reassembly struct {
	packetId    uint32
	startedAt   time.Time
	fragments   []*livekit.Fragment
	isCorrupted bool
	tqi         *utils.TimeoutQueueItem[*reassembly]
}

func (r *reassembly) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	e.AddUint32("packetId", r.packetId)
	e.AddTime("startAt", r.startedAt)
	e.AddDuration("age", time.Since(r.startedAt))

	expectedNumberOfFragments := len(r.fragments)
	expectedTotalSize := uint32(0)
	availableSize := uint32(0)
	var availableFragments []uint32
	for _, fragment := range r.fragments {
		if fragment == nil {
			continue
		}

		expectedTotalSize = fragment.TotalSize
		availableSize += fragment.FragmentSize
		availableFragments = append(availableFragments, fragment.FragmentNumber)
	}
	e.AddInt("expectedNumberOfFragments", expectedNumberOfFragments)
	e.AddUint32("expectedTotalSize", expectedTotalSize)
	e.AddUint32("availableSize", availableSize)
	e.AddArray("availableFragments", logger.Uint32Slice(availableFragments))

	e.AddBool("isCorrupted", r.isCorrupted)
	return nil
}

// ------------------------------------------------

type SignalReassemblerParams struct {
	Logger logger.Logger
}

type SignalReassembler struct {
	params SignalReassemblerParams

	lock         sync.Mutex
	reassemblies map[uint32]*reassembly

	timeoutQueue utils.TimeoutQueue[*reassembly]
}

func NewSignalReassembler(params SignalReassemblerParams) *SignalReassembler {
	return &SignalReassembler{
		params:       params,
		reassemblies: make(map[uint32]*reassembly),
	}
}

func (s *SignalReassembler) Reassemble(fragment *livekit.Fragment) []byte {
	s.lock.Lock()
	defer s.lock.Unlock()

	re, ok := s.reassemblies[fragment.PacketId]
	if !ok {
		re = &reassembly{
			packetId:  fragment.PacketId,
			startedAt: time.Now(),
			fragments: make([]*livekit.Fragment, fragment.NumFragments),
		}
		re.tqi = &utils.TimeoutQueueItem[*reassembly]{Value: re}

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
	delete(s.reassemblies, fragment.PacketId) // fully re-assembled, can be deleted from cache
	return data
}

func (s *SignalReassembler) Prune() {
	for it := s.timeoutQueue.IterateRemoveAfter(reassemblerTimeout); it.Next(); {
		re := it.Item().Value
		s.params.Logger.Infow("pruning stale reassembly packet", "reassembly", re)

		s.lock.Lock()
		delete(s.reassemblies, re.packetId)
		s.lock.Unlock()
	}
}

// SIGNALLING-V2-TODO: maybe do a prune worker? will need a way to stop/clean up the goroutine then
