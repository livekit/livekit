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
	"sync"

	"github.com/gammazero/deque"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

type SignalCacheParams struct {
	Logger         logger.Logger
	FirstMessageId uint32 // should be used for testing only
}

type SignalCache struct {
	params SignalCacheParams

	lock                         sync.Mutex
	messageId                    uint32
	lastProcessedRemoteMessageId uint32
	messages                     deque.Deque[*livekit.Signalv2ServerMessage]
}

func NewSignalCache(params SignalCacheParams) *SignalCache {
	s := &SignalCache{
		params:    params,
		messageId: params.FirstMessageId,
	}
	if s.messageId == 0 {
		s.messageId = uint32(rand.Intn(1<<8) + 1)
	}
	s.messages.SetBaseCap(16)
	return s
}

func (s *SignalCache) SetLastProcessedRemoteMessageId(lastProcessedRemoteMessageId uint32) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.lastProcessedRemoteMessageId = lastProcessedRemoteMessageId
}

func (s *SignalCache) Add(msg *livekit.Signalv2ServerMessage) *livekit.Signalv2ServerMessage {
	if msg != nil {
		s.AddBatch([]*livekit.Signalv2ServerMessage{msg})
	}

	return msg
}

// SIGNALLING-V2-TODO: may not need this API
func (s *SignalCache) AddBatch(msgs []*livekit.Signalv2ServerMessage) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, msg := range msgs {
		msg.Sequencer = &livekit.Sequencer{
			MessageId: s.messageId,
		}
		s.messageId++

		s.messages.PushBack(msg)
	}
}

func (s *SignalCache) Clear(till uint32) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.clearLocked(till)
}

func (s *SignalCache) clearLocked(till uint32) {
	for s.messages.Len() != 0 {
		front := s.messages.Front()
		if front.Sequencer.GetMessageId() > till {
			break
		}
		s.messages.PopFront()
	}
}

func (s *SignalCache) GetFromFront() []*livekit.Signalv2ServerMessage {
	s.lock.Lock()
	defer s.lock.Unlock()

	return s.getFromFrontLocked()
}

func (s *SignalCache) getFromFrontLocked() []*livekit.Signalv2ServerMessage {
	var msgs []*livekit.Signalv2ServerMessage
	for msg := range s.messages.Iter() {
		clone := utils.CloneProto(msg)
		clone.Sequencer.LastProcessedRemoteMessageId = s.lastProcessedRemoteMessageId
		msgs = append(msgs, clone)
	}

	return msgs
}

func (s *SignalCache) ClearAndGetFrom(from uint32) []*livekit.Signalv2ServerMessage {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.clearLocked(from - 1)
	return s.getFromFrontLocked()
}
