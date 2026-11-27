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
	"sync"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
)

const (
	maxSize = 100
)

type UserPacketDeduper struct {
	lock sync.Mutex
	seen map[uuid.UUID]struct{}

	history [maxSize]uuid.UUID
	next    int
}

func NewUserPacketDeduper() *UserPacketDeduper {
	return &UserPacketDeduper{
		seen: make(map[uuid.UUID]struct{}, maxSize),
	}
}

func (u *UserPacketDeduper) IsDuplicate(up *livekit.UserPacket) bool {
	id, err := uuid.FromBytes(up.Nonce)
	if err != nil {
		return false
	}

	u.lock.Lock()
	defer u.lock.Unlock()

	if _, ok := u.seen[id]; ok {
		return true
	}

	if len(u.seen) == maxSize {
		delete(u.seen, u.history[u.next])
	}
	u.seen[id] = struct{}{}
	u.history[u.next] = id
	u.next = (u.next + 1) % maxSize
	return false
}
